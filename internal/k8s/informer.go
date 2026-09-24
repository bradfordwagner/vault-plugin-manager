package k8s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"vault-plugin-manager/internal/logging"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// ConfigMapHandler receives events for the watched ConfigMap. OnChange fires on
// add and update (including informer resyncs); OnDelete fires on removal.
// OnWatchError fires when the underlying watch breaks in a way that is not part
// of normal churn — see benignWatchError.
type ConfigMapHandler struct {
	OnChange     func(*corev1.ConfigMap)
	OnDelete     func(namespace, name string)
	OnWatchError func(error)
}

// WatchConfigMap runs a shared informer scoped to a single ConfigMap (by name,
// via a field selector) and dispatches events to h. It blocks until the cache
// has synced, then returns the informer; events continue firing in the
// background until ctx is cancelled. The resync period drives periodic OnChange
// calls, which the caller uses to reconcile drift.
//
// The returned informer is what the health probes interrogate: IsStopped reports
// a watcher that has died outright, which no amount of waiting fixes.
func (c *Client) WatchConfigMap(ctx context.Context, ns, name string, resync time.Duration, h ConfigMapHandler) (cache.SharedIndexInformer, error) {
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
		return nil, fmt.Errorf("k8s: adding configmap event handler: %w", err)
	}

	// Must be installed before the informer starts. It replaces client-go's
	// default handler, so it logs as well as reporting.
	l := logging.Log().With("component", "configmap-informer")
	if err := informer.SetWatchErrorHandlerWithContext(func(_ context.Context, _ *cache.Reflector, err error) {
		if benignWatchError(err) {
			l.With("error", err).Debug("watch closed; relisting")
			return
		}
		l.With("error", err).Warn("configmap watch failed")
		if h.OnWatchError != nil {
			h.OnWatchError(err)
		}
	}); err != nil {
		return nil, fmt.Errorf("k8s: setting configmap watch error handler: %w", err)
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return nil, fmt.Errorf("k8s: configmap informer cache failed to sync")
	}
	return informer, nil
}

// benignWatchError reports whether err is normal watch churn rather than a
// broken watcher. The apiserver closes watches on a timer (client-go asks for a
// randomized 5-10m timeout) and ages out resource versions; both cases relist
// immediately, so treating them as failures would fire the probe constantly.
// Mirrors client-go's own DefaultWatchErrorHandler classification.
func benignWatchError(err error) bool {
	switch {
	case err == nil,
		errors.Is(err, io.EOF),
		errors.Is(err, io.ErrUnexpectedEOF),
		errors.Is(err, context.Canceled),
		apierrors.IsResourceExpired(err),
		apierrors.IsGone(err):
		return true
	default:
		return false
	}
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
