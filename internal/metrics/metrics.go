// Package metrics exposes the manager's Prometheus instrumentation.
//
// The manager serves no traffic, so its metrics describe the reconcile loop and
// the work it does against Vault rather than request handling. Three questions
// they are designed to answer:
//
//   - Is the loop still doing real work? A reconcile that succeeds against a
//     stale informer cache looks identical to a healthy one in the logs;
//     LastSuccessfulReconcile plus the per-pass counter make the difference
//     visible.
//   - Is the loop idempotent? At steady state the Vault action counters should
//     be flat. A steady drip of register/reload means an idempotency check is
//     missing its match (see the version-prefix note in CLAUDE.md).
//   - Is the spec the size we expect? A truncated or half-written ConfigMap
//     reconciles cleanly; SpecEntries shows the drop.
//
// Collectors are package-level on a private registry rather than injected
// through an interface. The reconciler's VaultOps/PodOps interfaces exist so
// tests can run without a cluster; metrics need no such escape hatch, and
// threading a recorder through every call site would cost more than it buys.
// Tests read the collectors back with prometheus/client_golang's testutil.
//
// No metric carries a pod name. Pod names change on every restart, so a
// pod-labelled series grows without bound and its history is useless.
package metrics

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Version is the build version reported by the build_info metric. It is
// overridden at link time with -X vault-plugin-manager/internal/metrics.Version.
var Version = "dev"

// Namespace prefixes every metric this package defines.
const Namespace = "vpm"

// Results recorded on the outcome label of ReconcileDone and VaultAction.
const (
	ResultSuccess = "success"
	ResultError   = "error"
	// ResultSkipped is a reconcile pass the Runner declined to make because the
	// ConfigMap was absent, empty, or unparseable. That is the user's spec being
	// wrong, not the manager being broken, so it is neither a success nor an
	// error and gets its own bucket.
	ResultSkipped = "skipped"
)

// Triggers recorded on the trigger label of ReconcileDone.
const (
	TriggerConfigMap = "configmap"
	TriggerResync    = "resync"
)

// Vault actions recorded on the action label of VaultAction. Reads
// (ListManagedMounts, ListRoles) are not actions and are not counted.
const (
	ActionRegister   = "register"
	ActionDeregister = "deregister"
	ActionMount      = "mount"
	ActionUnmount    = "unmount"
	ActionReload     = "reload"
	ActionRoleUpsert = "role_upsert"
	ActionRoleDelete = "role_delete"
)

// Kinds counted by the spec_entries gauge.
const (
	KindCatalog = "catalog"
	KindMounts  = "mounts"
	KindRoles   = "roles"
)

var (
	reg = prometheus.NewRegistry()

	pluginCopies = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "plugin_binary_copies_total",
		Help:      "Plugin binaries written to a Vault pod, counted once per pod placement.",
	}, []string{"plugin", "version"})

	reconciles = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "reconcile_total",
		Help:      "Reconcile passes by what triggered them and how they ended.",
	}, []string{"trigger", "result"})

	configMapChanges = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "configmap_changes_total",
		Help:      "Changes observed in the watched ConfigMap, by spec section and action.",
	}, []string{"section", "action"})

	vaultActions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: Namespace,
		Name:      "vault_actions_total",
		Help:      "Write operations issued against Vault. Flat at steady state; a steady rate means an idempotency check is missing.",
	}, []string{"action", "result"})

	reconcileDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: Namespace,
		Name:      "reconcile_duration_seconds",
		Help:      "Wall time of a reconcile pass.",
		// Spread wide: a pass is sub-second when nothing changed and minutes when
		// it fetches a large plugin and exec-copies it to every Vault pod.
		Buckets: []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
	})

	lastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "last_successful_reconcile_timestamp_seconds",
		Help:      "Unix time of the last clean reconcile pass. The freshness alert reads this.",
	})

	specEntries = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "spec_entries",
		Help:      "Entries in the parsed ConfigMap spec, by section.",
	}, []string{"kind"})

	buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: Namespace,
		Name:      "build_info",
		Help:      "Always 1; the version label carries the running build.",
	}, []string{"version"})
)

func init() {
	reg.MustRegister(
		pluginCopies,
		reconciles,
		configMapChanges,
		vaultActions,
		reconcileDuration,
		lastSuccess,
		specEntries,
		buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	buildInfo.WithLabelValues(Version).Set(1)

	// Pre-create the bounded label combinations the alerts read. A CounterVec
	// emits nothing until a label set is touched, so without this a brand-new
	// pod has no series at all and rate() queries match nothing -- indexes as
	// "no data" rather than "zero errors". Only the fully-enumerable counters
	// get this; plugin_binary_copies_total is keyed by plugin name and
	// configmap_changes_total is purely informational, so both stay lazy.
	for _, trigger := range []string{TriggerConfigMap, TriggerResync} {
		for _, result := range []string{ResultSuccess, ResultError, ResultSkipped} {
			reconciles.WithLabelValues(trigger, result)
		}
	}
	for _, action := range []string{
		ActionRegister, ActionDeregister, ActionMount,
		ActionUnmount, ActionReload, ActionRoleUpsert, ActionRoleDelete,
	} {
		for _, result := range []string{ResultSuccess, ResultError} {
			vaultActions.WithLabelValues(action, result)
		}
	}
}

// Handler serves the registry in the Prometheus text format.
func Handler() http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

// Gather returns the current value of every registered collector. It is what
// Handler serves, in structured form, for tests in other packages that need to
// assert on what an operator would scrape.
func Gather() ([]*dto.MetricFamily, error) { return reg.Gather() }

// CounterValue sums one counter family, keeping only series whose labels match
// every pair in want. A metric that has never been touched reads 0, so callers
// can take a before/after delta without special-casing the first observation.
func CounterValue(families []*dto.MetricFamily, name string, want map[string]string) float64 {
	var total float64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			matched := true
			for k, v := range want {
				if labels[k] != v {
					matched = false
					break
				}
			}
			if matched {
				total += m.GetCounter().GetValue()
			}
		}
	}
	return total
}

// RegisterHealth exposes the health package's token and watcher state as
// gauges. Both are read at scrape time rather than pushed, so the values cannot
// drift from health.State, which is authoritative for them.
//
// It is safe to call at most once; a second call panics on duplicate
// registration, which is the intended signal that the wiring is wrong.
func RegisterHealth(tokenValid, watcherRunning func() bool) {
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "vault_token_valid",
			Help:      "1 when the Vault token is valid, 0 while login or renewal is failing.",
		}, boolGauge(tokenValid)),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "configmap_watcher_running",
			Help:      "1 while the ConfigMap informer is running. 0 means the loop is reconciling a frozen cache.",
		}, boolGauge(watcherRunning)),
	)
}

func boolGauge(fn func() bool) func() float64 {
	return func() float64 {
		if fn() {
			return 1
		}
		return 0
	}
}

// PluginCopied records a plugin binary placed on one Vault pod.
func PluginCopied(plugin, version string) {
	pluginCopies.WithLabelValues(plugin, version).Inc()
}

// ReconcileDone records the outcome and duration of one reconcile pass. A
// ResultSuccess pass also stamps the freshness gauge.
func ReconcileDone(trigger, result string, d time.Duration) {
	reconciles.WithLabelValues(trigger, result).Inc()
	reconcileDuration.Observe(d.Seconds())
	if result == ResultSuccess {
		lastSuccess.SetToCurrentTime()
	}
}

// ConfigMapChange records one change reported by config.Diff.
func ConfigMapChange(section, action string) {
	configMapChanges.WithLabelValues(section, action).Inc()
}

// VaultAction records one write against Vault, deriving the result from err so
// call sites stay a single line. Use it only where reaching the call site
// already means a write was issued; an Ensure* that reports whether it wrote
// belongs in VaultActionIf.
func VaultAction(action string, err error) {
	result := ResultSuccess
	if err != nil {
		result = ResultError
	}
	vaultActions.WithLabelValues(action, result).Inc()
}

// VaultActionIf records an Ensure*-style call that may have been a no-op: it
// counts a failed attempt (the error is the thing to see) and a successful call
// that actually wrote, and stays silent when nothing was written. Counting the
// attempt instead would put three actions at a permanent non-zero rate and
// destroy the only signal this counter exists for -- "flat at steady state, a
// steady rate means an idempotency check is missing".
//
// The pre-seeded {action,result} series are untouched by this: they are created
// at init, so a no-op still reads 0 rather than "no data".
func VaultActionIf(action string, changed bool, err error) {
	if err == nil && !changed {
		return
	}
	VaultAction(action, err)
}

// SpecEntries records the size of the parsed spec.
func SpecEntries(catalog, mounts, roles int) {
	specEntries.WithLabelValues(KindCatalog).Set(float64(catalog))
	specEntries.WithLabelValues(KindMounts).Set(float64(mounts))
	specEntries.WithLabelValues(KindRoles).Set(float64(roles))
}
