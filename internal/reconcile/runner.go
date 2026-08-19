package reconcile

import (
	"context"
	"strings"
	"sync"
	"time"

	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/k8s"
	"vault-plugin-manager/internal/logging"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
)

// Runner wires the ConfigMap informer to the reconciler. It reconciles on every
// ConfigMap change and on a settings-driven resync interval, and serializes runs
// so only one reconcile is in flight at a time.
type Runner struct {
	rec  *Reconciler
	kc   *k8s.Client
	ns   string
	name string
	key  string
	log  *zap.SugaredLogger

	mu      sync.Mutex
	raw     string
	present bool

	trigger chan struct{}
}

// NewRunner builds a Runner for the ConfigMap ns/name and the data key holding
// the spec.
func NewRunner(rec *Reconciler, kc *k8s.Client, ns, name, key string) *Runner {
	return &Runner{
		rec:     rec,
		kc:      kc,
		ns:      ns,
		name:    name,
		key:     key,
		log:     logging.Log().With("component", "runner"),
		trigger: make(chan struct{}, 1),
	}
}

// Run starts the informer and the reconcile loop, blocking until ctx is cancelled.
func (ru *Runner) Run(ctx context.Context) error {
	handler := k8s.ConfigMapHandler{
		OnChange: func(cm *corev1.ConfigMap) {
			ru.set(cm.Data[ru.key], true)
			ru.notify()
		},
		OnDelete: func(_, _ string) {
			ru.set("", false)
			ru.notify()
		},
	}
	// resync=0: the informer only notifies on real changes; drift reconciles are
	// driven by our own timer below, whose interval is a live ConfigMap setting.
	if err := ru.kc.WatchConfigMap(ctx, ru.ns, ru.name, 0, handler); err != nil {
		return err
	}
	ru.log.With("namespace", ru.ns, "name", ru.name).Info("watching configmap")

	resync := config.DefaultResyncInterval
	timer := time.NewTimer(resync)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ru.trigger:
		case <-timer.C:
		}

		if spec, ok := ru.currentSpec(); ok {
			if err := logging.SetLevel(spec.Settings.LogLevel); err != nil {
				ru.log.With("error", err).Warn("invalid log level in settings")
			}
			if err := ru.rec.Reconcile(ctx, spec); err != nil {
				ru.log.With("error", err).Error("reconcile failed")
			} else {
				ru.log.Debug("reconcile complete")
			}
			resync = spec.Settings.ResyncInterval.Duration()
		}
		resetTimer(timer, resync)
	}
}

func (ru *Runner) set(raw string, present bool) {
	ru.mu.Lock()
	defer ru.mu.Unlock()
	ru.raw, ru.present = raw, present
}

// currentSpec parses the latest ConfigMap data. It returns ok=false (skipping
// reconcile) when the ConfigMap is absent, the data key is empty, or the spec is
// invalid — never reconciling an accidental empty spec into a full prune.
func (ru *Runner) currentSpec() (*config.Spec, bool) {
	ru.mu.Lock()
	raw, present := ru.raw, ru.present
	ru.mu.Unlock()

	if !present {
		ru.log.Warn("configmap absent; skipping reconcile")
		return nil, false
	}
	if strings.TrimSpace(raw) == "" {
		ru.log.With("key", ru.key).Warn("configmap data key empty; skipping reconcile")
		return nil, false
	}
	spec, err := config.Parse([]byte(raw))
	if err != nil {
		ru.log.With("error", err).Error("invalid configmap spec; skipping reconcile")
		return nil, false
	}
	return spec, true
}

// notify enqueues a reconcile without blocking; a pending trigger coalesces.
func (ru *Runner) notify() {
	select {
	case ru.trigger <- struct{}{}:
	default:
	}
}

// resetTimer safely resets t to fire after d.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}
