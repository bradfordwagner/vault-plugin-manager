package reconcile

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/k8s"
	"vault-plugin-manager/internal/logging"
	"vault-plugin-manager/internal/metrics"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
)

// Health receives the Runner's liveness and readiness signals.
// *health.State implements it; a nil Health is accepted and ignored.
type Health interface {
	// Heartbeat declares that the Runner's next signal is due within d.
	Heartbeat(d time.Duration)
	// ReconcileDone reports the outcome of one pass; nil means a clean pass.
	ReconcileDone(err error)
	// SetTokenGrace applies the ConfigMap's Vault-token health windows.
	SetTokenGrace(grace, failAfter time.Duration)
	// SetWatchGrace applies the ConfigMap's watch-error window.
	SetWatchGrace(grace time.Duration)
	// SetWatcherCheck installs an on-demand check for a stopped watcher.
	SetWatcherCheck(fn func() error)
	// WatchError reports a broken ConfigMap watch.
	WatchError(err error)
	// WatchHealthy reports a delivered event, which proves the watch works.
	WatchHealthy()
}

// noopHealth drops every signal, for callers (and tests) running without
// health probes.
type noopHealth struct{}

func (noopHealth) Heartbeat(time.Duration)          {}
func (noopHealth) ReconcileDone(error)              {}
func (noopHealth) SetTokenGrace(_, _ time.Duration) {}
func (noopHealth) SetWatchGrace(time.Duration)      {}
func (noopHealth) SetWatcherCheck(func() error)     {}
func (noopHealth) WatchError(error)                 {}
func (noopHealth) WatchHealthy()                    {}

// Runner wires the ConfigMap informer to the reconciler. It reconciles on every
// ConfigMap change and on a settings-driven resync interval, and serializes runs
// so only one reconcile is in flight at a time.
type Runner struct {
	rec  *Reconciler
	kc   *k8s.Client
	ns   string
	name string
	key  string
	h    Health
	log  *zap.SugaredLogger

	mu      sync.Mutex
	raw     string
	present bool

	trigger chan struct{}
}

// NewRunner builds a Runner for the ConfigMap ns/name and the data key holding
// the spec. h, which may be nil, receives the loop's liveness heartbeats,
// reconcile outcomes, and ConfigMap watcher state.
func NewRunner(rec *Reconciler, kc *k8s.Client, ns, name, key string, h Health) *Runner {
	if h == nil {
		h = noopHealth{}
	}
	return &Runner{
		rec:     rec,
		kc:      kc,
		ns:      ns,
		name:    name,
		key:     key,
		h:       h,
		log:     logging.Log().With("component", "runner"),
		trigger: make(chan struct{}, 1),
	}
}

// Run starts the informer and the reconcile loop, blocking until ctx is cancelled.
func (ru *Runner) Run(ctx context.Context) error {
	handler := k8s.ConfigMapHandler{
		// A delivered event proves the watch is working, so it clears any
		// recorded watch failure. It is NOT the only proof: a relist of an
		// UNCHANGED ConfigMap delivers no event here (client-go drops sync
		// notifications for a listener registered with resync=0), so recovery
		// is also inferred in health.State from failures going quiet.
		OnChange: func(cm *corev1.ConfigMap) {
			ru.h.WatchHealthy()
			ru.set(cm.Data[ru.key], true)
			ru.notify()
		},
		OnDelete: func(_, _ string) {
			ru.h.WatchHealthy()
			ru.set("", false)
			ru.notify()
		},
		OnWatchError: func(err error) { ru.h.WatchError(err) },
	}
	// resync=0: the informer only notifies on real changes; drift reconciles are
	// driven by our own timer below, whose interval is a live ConfigMap setting.
	informer, err := ru.kc.WatchConfigMap(ctx, ru.ns, ru.name, 0, handler)
	if err != nil {
		return err
	}
	// A stopped informer never restarts itself, and the loop below would happily
	// keep reconciling its stale cache, so this is a liveness failure.
	ru.h.SetWatcherCheck(func() error {
		if informer.IsStopped() {
			return errors.New("configmap informer stopped")
		}
		return nil
	})
	ru.log.With("namespace", ru.ns, "name", ru.name).Info("watching configmap")

	resync := config.DefaultResyncInterval
	stall := config.DefaultStallTimeout
	timer := time.NewTimer(resync)
	defer timer.Stop()

	// Liveness is a watchdog on this loop: before each wait and before each
	// pass we declare when the next signal is due, so a loop wedged on a hung
	// exec, fetch, or Vault call fails the probe instead of idling silently.
	ru.h.Heartbeat(resync + stall)

	// seen is the last spec whose changes were logged, so every ConfigMap edit
	// is reported exactly once even though the loop is level-triggered.
	var seen *config.Spec

	for {
		trigger := ""
		select {
		case <-ctx.Done():
			return nil
		case <-ru.trigger:
			trigger = metrics.TriggerConfigMap
		case <-timer.C:
			trigger = metrics.TriggerResync
		}

		// A pass is starting: it gets stall to finish, not the idle budget.
		ru.h.Heartbeat(stall)

		// An absent, empty, or invalid ConfigMap is the user's spec being
		// wrong, not the manager being wedged, so a skip still counts as a
		// clean pass for readiness.
		var err error
		started := time.Now()
		result := metrics.ResultSkipped
		if spec, ok := ru.currentSpec(); ok {
			if lvlErr := logging.SetLevel(spec.Settings.LogLevel); lvlErr != nil {
				ru.log.With("error", lvlErr).Warn("invalid log level in settings")
			}
			ru.logChanges(seen, spec, trigger)
			seen = spec
			metrics.SpecEntries(len(spec.Catalog), len(spec.Mounts), len(spec.Roles))
			if err = ru.rec.Reconcile(ctx, spec); err != nil {
				result = metrics.ResultError
				ru.log.With("error", err).Error("reconcile failed")
			} else {
				result = metrics.ResultSuccess
				ru.log.Debug("reconcile complete")
			}
			resync = spec.Settings.ResyncInterval.Duration()
			stall = spec.Settings.StallTimeout.Duration()
			ru.h.SetTokenGrace(spec.Settings.TokenGracePeriod.Duration(), spec.Settings.TokenFailTimeout.Duration())
			ru.h.SetWatchGrace(spec.Settings.WatchGracePeriod.Duration())
		} else {
			// Forget the spec so a ConfigMap that comes back is logged in full.
			seen = nil
		}
		// A skipped pass is recorded but deliberately does not stamp the
		// freshness gauge: an unparseable ConfigMap must not look like the
		// manager is keeping Vault up to date.
		metrics.ReconcileDone(trigger, result, time.Since(started))
		ru.h.ReconcileDone(err)
		ru.h.Heartbeat(resync + stall)
		resetTimer(timer, resync)
	}
}

// logChanges reports what moved in the ConfigMap before the reconcile acts on
// it: one line per change naming the section, the entry, and what differs. The
// reconciler then logs the work itself (copied binary, registered version,
// reconciled mount, pruned ...), so the two together read as intent followed by
// action. A pass with nothing new logs only at debug.
func (ru *Runner) logChanges(old, new *config.Spec, trigger string) {
	changes := config.Diff(old, new)
	if len(changes) == 0 {
		ru.log.With("trigger", trigger).Debug("reconciling; no configmap changes")
		return
	}
	for _, c := range changes {
		metrics.ConfigMapChange(c.Section, c.Action)
		ru.log.With(
			"section", c.Section,
			"key", c.Key,
			"action", c.Action,
			"detail", c.Detail,
		).Info("configmap change: " + c.String())
	}
	ru.log.With("trigger", trigger, "changes", len(changes)).Info("reconciling configmap changes")
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
