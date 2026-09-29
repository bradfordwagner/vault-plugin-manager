package metrics

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The collectors are package-level, so counters carry across tests within this
// package and cannot be reset. Every assertion below therefore measures a
// delta, never an absolute.

func TestHandlerServesTheRegistry(t *testing.T) {
	PluginCopied("example", "1.0.0")

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"vpm_plugin_binary_copies_total",
		"vpm_reconcile_total",
		"vpm_vault_actions_total",
		"vpm_reconcile_duration_seconds",
		"vpm_build_info",
		// The runtime collectors are what answer "is it leaking".
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics body is missing %s", want)
		}
	}
}

// The alerting counters must expose their series before anything has happened,
// so a healthy new pod reads as zero errors rather than as no data.
func TestAlertingCountersArePreSeeded(t *testing.T) {
	body := scrape(t)
	for _, want := range []string{
		`vpm_reconcile_total{result="error",trigger="resync"} 0`,
		`vpm_vault_actions_total{action="reload",result="error"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("want a pre-seeded zero series:\n  %s", want)
		}
	}
	// Counted the other way: configmap_changes_total is deliberately lazy.
	if strings.Contains(body, "vpm_configmap_changes_total") {
		t.Error("configmap_changes_total should not exist before any change is recorded")
	}
	ConfigMapChange("catalog", "added")
	if !strings.Contains(scrape(t), "vpm_configmap_changes_total") {
		t.Error("configmap_changes_total should appear once a change is recorded")
	}
}

func TestBuildInfoCarriesTheVersion(t *testing.T) {
	if got := testutil.ToFloat64(buildInfo.WithLabelValues(Version)); got != 1 {
		t.Fatalf("want build_info{version=%q}=1, got %v", Version, got)
	}
}

func TestPluginCopiedCountsPerNameAndVersion(t *testing.T) {
	before := testutil.ToFloat64(pluginCopies.WithLabelValues("counted", "2.0.0"))
	PluginCopied("counted", "2.0.0")
	PluginCopied("counted", "2.0.0")
	if got := testutil.ToFloat64(pluginCopies.WithLabelValues("counted", "2.0.0")) - before; got != 2 {
		t.Fatalf("want 2 copies recorded, got %v", got)
	}
}

func TestVaultActionDerivesResultFromError(t *testing.T) {
	okBefore := testutil.ToFloat64(vaultActions.WithLabelValues(ActionReload, ResultSuccess))
	errBefore := testutil.ToFloat64(vaultActions.WithLabelValues(ActionReload, ResultError))

	VaultAction(ActionReload, nil)
	VaultAction(ActionReload, errors.New("vault is down"))

	if got := testutil.ToFloat64(vaultActions.WithLabelValues(ActionReload, ResultSuccess)) - okBefore; got != 1 {
		t.Errorf("want 1 success, got %v", got)
	}
	if got := testutil.ToFloat64(vaultActions.WithLabelValues(ActionReload, ResultError)) - errBefore; got != 1 {
		t.Errorf("want 1 error, got %v", got)
	}
}

// VaultActionIf is what keeps the counter honest: it records a write, not an
// attempt. A no-op Ensure* must leave the series exactly where it was, or every
// idempotent action sits at a permanent non-zero rate and the "flat at steady
// state" reading of this metric is worthless.
func TestVaultActionIfOnlyCountsRealWrites(t *testing.T) {
	ok := func() float64 {
		return testutil.ToFloat64(vaultActions.WithLabelValues(ActionRegister, ResultSuccess))
	}
	bad := func() float64 {
		return testutil.ToFloat64(vaultActions.WithLabelValues(ActionRegister, ResultError))
	}
	okBefore, errBefore := ok(), bad()

	VaultActionIf(ActionRegister, false, nil)                // no-op: records nothing
	VaultActionIf(ActionRegister, true, nil)                 // wrote
	VaultActionIf(ActionRegister, false, errors.New("boom")) // failed attempt still counts

	if got := ok() - okBefore; got != 1 {
		t.Errorf("want 1 success (the write only), got %v", got)
	}
	if got := bad() - errBefore; got != 1 {
		t.Errorf("want 1 error, got %v", got)
	}
}

// A skipped pass must not stamp the freshness gauge: an unparseable ConfigMap
// cannot be allowed to look like the manager is keeping Vault up to date.
func TestOnlySuccessStampsFreshness(t *testing.T) {
	ReconcileDone("resync", ResultSuccess, time.Second)
	stamped := testutil.ToFloat64(lastSuccess)
	if stamped == 0 {
		t.Fatal("want the freshness gauge stamped after a successful pass")
	}

	ReconcileDone("resync", ResultSkipped, time.Second)
	ReconcileDone("configmap", ResultError, time.Second)
	if got := testutil.ToFloat64(lastSuccess); got != stamped {
		t.Fatalf("skipped/error passes moved the freshness gauge: %v -> %v", stamped, got)
	}
}

func TestSpecEntriesTracksTheLatestSpec(t *testing.T) {
	SpecEntries(3, 2, 1)
	SpecEntries(1, 0, 0) // a shrunken spec must be visible, not sticky
	for _, tc := range []struct {
		kind string
		want float64
	}{{KindCatalog, 1}, {KindMounts, 0}, {KindRoles, 0}} {
		if got := testutil.ToFloat64(specEntries.WithLabelValues(tc.kind)); got != tc.want {
			t.Errorf("spec_entries{kind=%s} = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

func TestRegisterHealthReadsThroughAtScrapeTime(t *testing.T) {
	tokenOK, watcherOK := false, true
	RegisterHealth(func() bool { return tokenOK }, func() bool { return watcherOK })

	if got := gaugeValue(t, "vpm_vault_token_valid"); got != 0 {
		t.Fatalf("want token gauge 0, got %v", got)
	}

	// Flipping the source must move the gauge with no further registration:
	// the value is pulled, not pushed.
	tokenOK, watcherOK = true, false
	if got := gaugeValue(t, "vpm_vault_token_valid"); got != 1 {
		t.Errorf("want token gauge 1 after the source flipped, got %v", got)
	}
	if got := gaugeValue(t, "vpm_configmap_watcher_running"); got != 0 {
		t.Errorf("want watcher gauge 0 after the source flipped, got %v", got)
	}
}

func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// gaugeValue scrapes the registry and reads back one unlabelled gauge.
func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		if len(f.GetMetric()) != 1 {
			t.Fatalf("want exactly one series for %s, got %d", name, len(f.GetMetric()))
		}
		return f.GetMetric()[0].GetGauge().GetValue()
	}
	t.Fatalf("metric %s was never registered", name)
	return 0
}
