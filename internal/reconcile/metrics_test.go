package reconcile

import (
	"context"
	"testing"

	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/metrics"
	"vault-plugin-manager/internal/vault"
)

// The metrics collectors are package-level and cannot be reset between tests,
// so these assert deltas around a reconcile rather than absolute values.

// counters snapshots the series this file asserts on.
type counters struct {
	copies     float64
	register   float64
	mount      float64
	reload     float64
	unmount    float64
	deregister float64
	roleUpsert float64
}

// snapshotCounters reads the collectors back the way an operator would: from
// the registry that Handler serves.
func snapshotCounters(t *testing.T) counters {
	t.Helper()
	families, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	// Only successes are asserted: a failed action in these fakes would fail
	// the reconcile itself, so the test would never reach the comparison.
	action := func(name string) float64 {
		return metrics.CounterValue(families, "vpm_vault_actions_total",
			map[string]string{"action": name, "result": metrics.ResultSuccess})
	}
	return counters{
		copies:     metrics.CounterValue(families, "vpm_plugin_binary_copies_total", nil),
		register:   action(metrics.ActionRegister),
		mount:      action(metrics.ActionMount),
		reload:     action(metrics.ActionReload),
		unmount:    action(metrics.ActionUnmount),
		deregister: action(metrics.ActionDeregister),
		roleUpsert: action(metrics.ActionRoleUpsert),
	}
}

func TestReconcileRecordsCopiesAndVaultActions(t *testing.T) {
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog: []config.CatalogEntry{{
			Name: "vault-plugin-secrets-metrics", Type: config.PluginTypeSecret, Version: "0.1.0",
			Source: config.Source{URL: "https://x/m.zip"},
		}},
		Mounts: []config.MountEntry{{
			Path: "m", Plugin: "vault-plugin-secrets-metrics", Type: config.PluginTypeSecret, Version: "0.1.0",
		}},
	}
	// Two pods: the copy counter counts per pod placement, not per plugin.
	pods := &fakePods{pods: []string{"vault-0", "vault-1"}}
	r := New(&fakeVault{}, pods, fakeFetcher{}, testConfig())

	before := snapshotCounters(t)
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	after := snapshotCounters(t)

	for _, tc := range []struct {
		name string
		got  float64
		want float64
	}{
		{"binary copies", after.copies - before.copies, 2},
		{"register", after.register - before.register, 1},
		{"mount", after.mount - before.mount, 1},
		{"reload", after.reload - before.reload, 1},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: recorded %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// Pruning is the path where a miscounted action is easiest to miss, because
// the work happens inside prune() rather than the main pass.
func TestPruneRecordsVaultActions(t *testing.T) {
	fv := &fakeVault{managed: []vault.ManagedMount{{
		Path: "gone", Type: "secret", Plugin: "vault-plugin-secrets-gone", Version: "0.1.0",
	}}}
	r := New(fv, &fakePods{pods: []string{"vault-0"}}, fakeFetcher{}, testConfig())
	spec := &config.Spec{Settings: config.Settings{PruneMode: config.PruneFull}}

	before := snapshotCounters(t)
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	after := snapshotCounters(t)

	if got := after.unmount - before.unmount; got != 1 {
		t.Errorf("unmount: recorded %v, want 1", got)
	}
	if got := after.deregister - before.deregister; got != 1 {
		t.Errorf("deregister: recorded %v, want 1", got)
	}
}

// A second reconcile against an unchanged spec writes nothing to Vault, so it
// must record nothing. This is the regression for the counters having been
// wired above the `changed` branch: they then climbed on every pass, which both
// hid a real re-registration storm in the floor and made
// "a steady rate means a missing idempotency check" permanently false.
func TestReconcileSteadyStateRecordsNoVaultActions(t *testing.T) {
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog: []config.CatalogEntry{{
			Name: "vault-plugin-secrets-steady", Type: config.PluginTypeSecret, Version: "0.1.0",
			Source: config.Source{URL: "https://x/s.zip"},
		}},
		Mounts: []config.MountEntry{{
			Path: "s", Plugin: "vault-plugin-secrets-steady", Type: config.PluginTypeSecret, Version: "0.1.0",
		}},
		Roles: []config.RoleEntry{
			{Mount: "s", RolesPath: "roles", Name: "reader", Data: map[string]any{"ttl": "1h"}},
		},
	}
	fv := &fakeVault{}
	// Live roles must include the declared one, or the prune pass has nothing to
	// list; it must not be deleted either, since the spec declares it.
	fv.rolesByPath = map[string][]string{"s/roles": {"reader"}}
	fv.managed = []vault.ManagedMount{{
		Path: "s", Type: "secret", Plugin: "vault-plugin-secrets-steady", Version: "0.1.0",
	}}
	r := New(fv, &fakePods{pods: []string{"vault-0"}}, fakeFetcher{}, testConfig())

	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatalf("first pass: %v", err)
	}

	before := snapshotCounters(t)
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	after := snapshotCounters(t)

	for _, tc := range []struct {
		name string
		got  float64
	}{
		{"binary copies", after.copies - before.copies},
		{"register", after.register - before.register},
		{"mount", after.mount - before.mount},
		{"reload", after.reload - before.reload},
		{"role_upsert", after.roleUpsert - before.roleUpsert},
		{"unmount", after.unmount - before.unmount},
		{"deregister", after.deregister - before.deregister},
	} {
		if tc.got != 0 {
			t.Errorf("%s: recorded %v on an unchanged spec, want 0", tc.name, tc.got)
		}
	}
}

// The first pass still counts what it really wrote, roles included.
func TestReconcileRecordsRoleUpsert(t *testing.T) {
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneNever},
		Roles: []config.RoleEntry{
			{Mount: "r", RolesPath: "roles", Name: "one", Data: map[string]any{"ttl": "1h"}},
			{Mount: "r", RolesPath: "roles", Name: "two", Data: map[string]any{"ttl": "2h"}},
		},
	}
	r := New(&fakeVault{}, &fakePods{pods: []string{"vault-0"}}, fakeFetcher{}, testConfig())

	before := snapshotCounters(t)
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	after := snapshotCounters(t)

	if got := after.roleUpsert - before.roleUpsert; got != 2 {
		t.Errorf("role_upsert: recorded %v, want 2", got)
	}
}
