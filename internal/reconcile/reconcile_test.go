package reconcile

import (
	"context"
	"testing"

	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/fetch"
	"vault-plugin-manager/internal/vault"
)

// --- fakes ---

type fakeFetcher struct{}

func (fakeFetcher) Fetch(_ context.Context, req fetch.Request) (*fetch.Result, error) {
	return &fetch.Result{Binary: []byte("bin:" + req.URL + req.Image), SHA256: "sha-" + req.URL + req.Image}, nil
}

type fakePods struct {
	pods    []string
	ensured []string // "pod:path"
	removed []string // "pod:path"
}

func (f *fakePods) ListRunningPods(_ context.Context, _, _ string) ([]string, error) {
	return f.pods, nil
}
func (f *fakePods) EnsureFile(_ context.Context, _, pod, _, path string, _ []byte, _, _ string) (bool, error) {
	f.ensured = append(f.ensured, pod+":"+path)
	return true, nil
}
func (f *fakePods) RemoveFile(_ context.Context, _, pod, _, path string) error {
	f.removed = append(f.removed, pod+":"+path)
	return nil
}

type fakeVault struct {
	registered   []string // "name@version"
	deregistered []string // "name@version"
	mounts       []string // "type:path@version"
	disabled     []string // "type:path"
	reloaded     []string
	managed      []vault.ManagedMount
}

func (f *fakeVault) EnsurePlugin(_ context.Context, p vault.Plugin) (bool, error) {
	f.registered = append(f.registered, p.Name+"@"+p.Version)
	return true, nil
}
func (f *fakeVault) DeregisterPlugin(_ context.Context, name, _, version string) error {
	f.deregistered = append(f.deregistered, name+"@"+version)
	return nil
}
func (f *fakeVault) EnsureMount(_ context.Context, m vault.Mount) (bool, error) {
	f.mounts = append(f.mounts, m.Type+":"+m.Path+"@"+m.Version)
	return true, nil
}
func (f *fakeVault) DisableMount(_ context.Context, path, mountType string) error {
	f.disabled = append(f.disabled, mountType+":"+path)
	return nil
}
func (f *fakeVault) ListManagedMounts(_ context.Context) ([]vault.ManagedMount, error) {
	return f.managed, nil
}
func (f *fakeVault) ReloadPlugin(_ context.Context, name string) error {
	f.reloaded = append(f.reloaded, name)
	return nil
}

func has(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func testConfig() Config {
	return Config{VaultNamespace: "vault", VaultPodSelector: "app=vault", VaultContainer: "vault", PluginDir: "/vault/plugins"}
}

// --- tests ---

func TestReconcileHappyPath(t *testing.T) {
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog: []config.CatalogEntry{{
			Name: "vault-plugin-secrets-foo", Type: config.PluginTypeSecret, Version: "0.3.1",
			Source: config.Source{URL: "https://x/foo.zip"},
		}},
		Mounts: []config.MountEntry{{
			Path: "foo", Plugin: "vault-plugin-secrets-foo", Type: config.PluginTypeSecret, Version: "0.3.1",
		}},
	}
	pods := &fakePods{pods: []string{"vault-0", "vault-1"}}
	fv := &fakeVault{}
	r := New(fv, pods, fakeFetcher{}, testConfig())

	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	// Binary placed on both pods at the versioned path.
	for _, pod := range []string{"vault-0", "vault-1"} {
		want := pod + ":/vault/plugins/vault-plugin-secrets-foo-0.3.1"
		if !has(pods.ensured, want) {
			t.Errorf("missing EnsureFile %q; got %v", want, pods.ensured)
		}
	}
	if !has(fv.registered, "vault-plugin-secrets-foo@0.3.1") {
		t.Errorf("plugin not registered; got %v", fv.registered)
	}
	if !has(fv.mounts, "secret:foo@0.3.1") {
		t.Errorf("mount not reconciled; got %v", fv.mounts)
	}
	if !has(fv.reloaded, "vault-plugin-secrets-foo") {
		t.Errorf("plugin not reloaded; got %v", fv.reloaded)
	}
}

func TestReconcilePruneFull(t *testing.T) {
	// Live Vault has a managed mount that is not in the (empty) desired spec.
	fv := &fakeVault{managed: []vault.ManagedMount{
		{Path: "old", Type: "secret", Plugin: "vault-plugin-secrets-old", Version: "1.0.0"},
	}}
	pods := &fakePods{pods: []string{"vault-0"}}
	r := New(fv, pods, fakeFetcher{}, testConfig())

	spec := &config.Spec{Settings: config.Settings{PruneMode: config.PruneFull}}
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	if !has(fv.disabled, "secret:old") {
		t.Errorf("mount not disabled; got %v", fv.disabled)
	}
	if !has(fv.deregistered, "vault-plugin-secrets-old@1.0.0") {
		t.Errorf("version not deregistered; got %v", fv.deregistered)
	}
	if !has(pods.removed, "vault-0:/vault/plugins/vault-plugin-secrets-old-1.0.0") {
		t.Errorf("binary not removed; got %v", pods.removed)
	}
}

func TestReconcilePruneDeregisterKeepsBinary(t *testing.T) {
	fv := &fakeVault{managed: []vault.ManagedMount{
		{Path: "old", Type: "secret", Plugin: "p", Version: "1.0.0"},
	}}
	pods := &fakePods{pods: []string{"vault-0"}}
	r := New(fv, pods, fakeFetcher{}, testConfig())

	spec := &config.Spec{Settings: config.Settings{PruneMode: config.PruneDeregister}}
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	if !has(fv.disabled, "secret:old") || !has(fv.deregistered, "p@1.0.0") {
		t.Errorf("expected disable+deregister; disabled=%v deregistered=%v", fv.disabled, fv.deregistered)
	}
	if len(pods.removed) != 0 {
		t.Errorf("deregister mode must not remove binaries; got %v", pods.removed)
	}
}

func TestReconcilePruneNever(t *testing.T) {
	fv := &fakeVault{managed: []vault.ManagedMount{
		{Path: "old", Type: "secret", Plugin: "p", Version: "1.0.0"},
	}}
	pods := &fakePods{pods: []string{"vault-0"}}
	r := New(fv, pods, fakeFetcher{}, testConfig())

	spec := &config.Spec{Settings: config.Settings{PruneMode: config.PruneNever}}
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	if len(fv.disabled) != 0 || len(fv.deregistered) != 0 || len(pods.removed) != 0 {
		t.Errorf("never mode must not prune; disabled=%v deregistered=%v removed=%v", fv.disabled, fv.deregistered, pods.removed)
	}
}

func TestReconcilePruneKeepsStillDesiredVersion(t *testing.T) {
	// The managed mount "old" is being removed, but another desired mount still
	// uses the same plugin@version, so the version must NOT be deregistered.
	fv := &fakeVault{managed: []vault.ManagedMount{
		{Path: "old", Type: "secret", Plugin: "p", Version: "1.0.0"},
	}}
	pods := &fakePods{pods: []string{"vault-0"}}
	r := New(fv, pods, fakeFetcher{}, testConfig())

	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog:  []config.CatalogEntry{{Name: "p", Type: config.PluginTypeSecret, Version: "1.0.0", Source: config.Source{URL: "https://x/p"}}},
		Mounts:   []config.MountEntry{{Path: "keep", Plugin: "p", Type: config.PluginTypeSecret, Version: "1.0.0"}},
	}
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	if !has(fv.disabled, "secret:old") {
		t.Errorf("stale mount should still be disabled; got %v", fv.disabled)
	}
	if len(fv.deregistered) != 0 {
		t.Errorf("version still in use must not be deregistered; got %v", fv.deregistered)
	}
	if len(pods.removed) != 0 {
		t.Errorf("binary still in use must not be removed; got %v", pods.removed)
	}
}

func TestReconcileNoPodsIsError(t *testing.T) {
	r := New(&fakeVault{}, &fakePods{pods: nil}, fakeFetcher{}, testConfig())
	if err := r.Reconcile(context.Background(), &config.Spec{Settings: config.Settings{PruneMode: config.PruneNever}}); err == nil {
		t.Fatal("expected error when no vault pods are found")
	}
}
