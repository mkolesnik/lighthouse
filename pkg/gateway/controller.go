package gateway

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog"
)

type Controller struct {
	// Indirection hook for unit tests to supply fake client sets
	newClientset func(kubeConfig *rest.Config) (dynamic.Interface, error)
	informer     cache.Controller
	store        cache.Store
	queue        workqueue.RateLimitingInterface
	stopCh       chan struct{}
	gwStatusMap  *Map
}

func NewController(gwMap *Map) *Controller {
	return &Controller{
		queue: workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		newClientset: func(c *rest.Config) (dynamic.Interface, error) {
			return dynamic.NewForConfig(c)

		},
		stopCh:      make(chan struct{}),
		gwStatusMap: gwMap,
	}
}

func (c *Controller) Start(kubeConfig *rest.Config) error {
	klog.Infof("Starting Gateways Controller")

	gwClientset, err := getCheckedClientset(kubeConfig)
	if err != nil {
		return err
	}
	c.store, c.informer = cache.NewInformer(&cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return gwClientset.List(metav1.ListOptions{})
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return gwClientset.Watch(options)
		},
	}, &unstructured.Unstructured{}, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				c.queue.Add(key)
			}
		},
		UpdateFunc: func(obj interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			klog.V(2).Infof("GatewayStatus %q updated", key)
			if err == nil {
				c.queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			klog.V(2).Infof("GatewayStatus %q deleted", key)
			if err == nil {
				c.gatewayDeleted(obj, key)
			}
		},
	})
	go c.informer.Run(c.stopCh)
	go c.runWorker()

	return nil
}

func (c *Controller) Stop() {
	close(c.stopCh)
	c.queue.ShutDown()

	klog.Infof("ServiceImport Controller stopped")
}

func (c *Controller) runWorker() {
	for {
		keyObj, shutdown := c.queue.Get()
		if shutdown {
			klog.Infof("Lighthouse watcher for Gateways stopped")
			return
		}

		key := keyObj.(string)
		func() {
			defer c.queue.Done(key)
			obj, exists, err := c.store.GetByKey(key)
			if err != nil {
				klog.Errorf("Error retrieving gateway with key %q from the cache: %v", key, err)
				// requeue the item to work on later
				c.queue.AddRateLimited(key)
				return
			}
			if exists {
				c.gatewayCreatedOrUpdated(obj)
			}
			c.queue.Forget(key)
		}()
	}
}

func (c *Controller) gatewayDeleted(obj interface{}, key string) {
	var ok bool
	if _, ok = obj.(*unstructured.Unstructured); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Could not convert object %v to DeletedFinalStateUnknown", obj)
			return
		}
		_, ok = tombstone.Obj.(*unstructured.Unstructured)
		if !ok {
			klog.Errorf("Could not convert object tombstone %v to Unstructured", tombstone.Obj)
			return
		}
	}
	key, _ = cache.DeletionHandlingMetaNamespaceKeyFunc(obj)

	haStatus, _, _ := getGatewayStatus(obj)
	if haStatus == "active" {
		c.gwStatusMap.Store(make(map[string]bool))
	}
}

func (c *Controller) gatewayCreatedOrUpdated(obj interface{}) {

	haStatus, connections, ok := getGatewayStatus(obj)
	if !ok || haStatus != "active" {
		return
	}
	var newMap map[string]bool
	currentMap := c.gwStatusMap.Get()
	for _, connection := range connections {
		connectionMap := connection.(map[string]interface{})

		status, found, err := unstructured.NestedString(connectionMap, "status")
		if err != nil || !found {
			klog.Errorf("status field not found in %#v", connectionMap)
		}
		clusterId, found, err := unstructured.NestedString(connectionMap, "endpoint", "cluster_id")
		if !found || err != nil {
			klog.Errorf("clusterId field not found in %#v", connectionMap)
			return
		}

		if status == "connected" {
			_, found := currentMap[clusterId]
			if !found {
				if newMap == nil {
					newMap = copyMap(currentMap)
				}
				newMap[clusterId] = true
			}
		} else {
			_, found = currentMap[clusterId]
			if found {
				if newMap == nil {
					newMap = copyMap(currentMap)
				}
				delete(newMap, clusterId)
			}
		}

	}
	if newMap != nil {
		klog.Errorf("Updating the gateway status %#v", newMap)
		c.gwStatusMap.Store(newMap)
	}
}

func getGatewayStatus(obj interface{}) (string, []interface{}, bool) {
	status, found, err := unstructured.NestedMap(obj.(*unstructured.Unstructured).Object, "status")
	if !found || err != nil {
		klog.Errorf("status field not found in %#v", obj)
		return "", nil, false
	}
	haStatus, found, err := unstructured.NestedString(status, "haStatus")
	if !found || err != nil {
		klog.Errorf("haStatus field not found in %#v, found, err", status, found, err)
		return "", nil, false
	}
	connections, found, err := unstructured.NestedSlice(status, "connections")
	if !found || err != nil {
		klog.Errorf("connections field not found in %#v, found, err", status, found, err)
		return haStatus, nil, false
	}
	return haStatus, connections, true
}

func (c *Controller) getClusterStatus(clusterId string) bool {
	gwMap := c.gwStatusMap.Get()
	return gwMap[clusterId]
}

func getCheckedClientset(kubeConfig *rest.Config) (dynamic.ResourceInterface, error) {
	clientSet, err := dynamic.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating client set: %v", err)
	}
	gvr, _ := schema.ParseResourceArg("gateways.v1.submariner.io")
	gwClient := clientSet.Resource(*gvr).Namespace(v1.NamespaceAll)
	_, err = gwClient.List(metav1.ListOptions{})

	return gwClient, err
}

func copyMap(src map[string]bool) map[string]bool {
	m := make(map[string]bool)
	for k, v := range src {
		m[k] = v
	}
	return m
}