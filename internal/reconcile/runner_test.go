package reconcile

import (
	"testing"

	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/logging"
	"vault-plugin-manager/internal/metrics"
)

func changeCount(t *testing.T) float64 {
	t.Helper()
	families, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	return metrics.CounterValue(families, "vpm_configmap_changes_total", nil)
}

// A change whose reconcile FAILS is logged against the last APPLIED spec, so it
// is named again on every retry rather than scrolling away after one line — but
// it is counted against the last spec merely OBSERVED, so retrying does not
// inflate configmap_changes_total.
func TestLogChangesCountsOncePerChange(t *testing.T) {
	ru := &Runner{log: logging.Log().With("component", "runner-test")}
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog: []config.CatalogEntry{{
			Name: "vault-plugin-secrets-log", Type: config.PluginTypeSecret, Version: "0.1.0",
			Source: config.Source{URL: "https://x/l.zip"},
		}},
	}

	before := changeCount(t)
	ru.logChanges(nil, spec, metrics.TriggerConfigMap, nil)
	firstPass := changeCount(t) - before
	if firstPass == 0 {
		t.Fatal("a new spec must count its changes")
	}

	// The reconcile failed, so `applied` stays nil and the same changes are
	// reported again; `counted` has advanced, so nothing is counted twice.
	before = changeCount(t)
	ru.logChanges(nil, spec, metrics.TriggerResync, spec)
	if got := changeCount(t) - before; got != 0 {
		t.Errorf("a retried change was counted again: %v", got)
	}

	// A genuinely new change on top of the pending one still counts.
	next := &config.Spec{
		Settings: spec.Settings,
		Catalog:  spec.Catalog,
		Mounts: []config.MountEntry{{
			Path: "l", Plugin: "vault-plugin-secrets-log", Type: config.PluginTypeSecret, Version: "0.1.0",
		}},
	}
	before = changeCount(t)
	ru.logChanges(nil, next, metrics.TriggerResync, spec)
	if got := changeCount(t) - before; got == 0 {
		t.Error("a new change on top of a pending one must still be counted")
	}
}
