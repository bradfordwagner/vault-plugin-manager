package k8s

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// ConfigMapHandler receives events for the watched ConfigMap. OnChange fires on
// add and update (including informer resyncs); OnDelete fires on removal.
type ConfigMapHandler struct {
	OnChange func(*corev1.ConfigMap)
	OnDelete func(namespace, name string)
}

// WatchConfigMap runs a shared informer scoped to a single ConfigMap (by name,
// via a field selector) and dispatches events to h. It blocks until the cache
// has synced, then returns; events continue firing in the background until ctx
// is cancelled. The resync period drives periodic OnChange calls, which the
// caller uses to reconcile drift.
func (c *Client) WatchConfigMap(ctx context.Context, ns, name string, resync time.Duration, h ConfigMapHandler) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		c.clientset,
		resync,
		informers.WithNamespace(ns),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", name).String()
		}),
	)
	informer := factory.Core().V1().ConfigMaps().Informer()

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if cm := asConfigMap(obj); cm != nil && cm.Name == name && h.OnChange != nil {
				h.OnChange(cm)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if cm := asConfigMap(newObj); cm != nil && cm.Name == name && h.OnChange != nil {
				h.OnChange(cm)
			}
		},
		DeleteFunc: func(obj interface{}) {
			if cm := asConfigMap(obj); cm != nil && cm.Name == name && h.OnDelete != nil {
				h.OnDelete(cm.Namespace, cm.Name)
			}
		},
	}); err != nil {
		return fmt.Errorf("k8s: adding configmap event handler: %w", err)
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("k8s: configmap informer cache failed to sync")
	}
	return nil
}

// asConfigMap extracts a ConfigMap from an informer object, unwrapping the
// tombstone delivered on some delete events.
func asConfigMap(obj interface{}) *corev1.ConfigMap {
	if cm, ok := obj.(*corev1.ConfigMap); ok {
		return cm
	}
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if cm, ok := tombstone.Obj.(*corev1.ConfigMap); ok {
			return cm
		}
	}
	return nil
}
