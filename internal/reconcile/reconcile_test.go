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
	placed  map[string]bool
}

func (f *fakePods) ListRunningPods(_ context.Context, _, _ string) ([]string, error) {
	return f.pods, nil
}

// EnsureFile records every call but reports copied only the first time a given
// pod+path is placed, so a second reconcile against an unchanged spec is a
// no-op the way the real exec-copy transport is.
func (f *fakePods) EnsureFile(_ context.Context, _, pod, _, path string, _ []byte, _, _ string) (bool, error) {
	key := pod + ":" + path
	f.ensured = append(f.ensured, key)
	return f.firstTime(key), nil
}

// firstTime reports whether key is being written for the first time.
func (f *fakePods) firstTime(key string) bool {
	if f.placed == nil {
		f.placed = map[string]bool{}
	}
	if f.placed[key] {
		return false
	}
	f.placed[key] = true
	return true
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

	// Role recording.
	ensuredRoles []string            // "mount/rolesPath/name"
	deletedRoles []string            // "mount/rolesPath/name"
	rolesByPath  map[string][]string // existing roles keyed by "mount/rolesPath" for ListRoles
	calls        []string            // ordered op log for cross-phase assertions

	// mountResult, when set, replaces what EnsureMount reports, so a test can
	// express a change that must NOT reload (description/options drift).
	mountResult *vault.MountResult

	// live is what the fake Vault already holds, so the Ensure* methods report
	// changed only on the pass that actually writes -- the real clients are
	// read-compare-write, and a fake that always reports changed cannot catch a
	// counter wired above the changed branch.
	live map[string]bool
}

// wrote records key and reports whether this call is the one that wrote it.
func (f *fakeVault) wrote(key string) bool {
	if f.live == nil {
		f.live = map[string]bool{}
	}
	if f.live[key] {
		return false
	}
	f.live[key] = true
	return true
}

func (f *fakeVault) EnsurePlugin(_ context.Context, p vault.Plugin) (bool, error) {
	key := p.Name + "@" + p.Version
	f.registered = append(f.registered, key)
	return f.wrote("plugin:" + key), nil
}
func (f *fakeVault) DeregisterPlugin(_ context.Context, name, _, version string) error {
	f.deregistered = append(f.deregistered, name+"@"+version)
	return nil
}
func (f *fakeVault) EnsureMount(_ context.Context, m vault.Mount) (vault.MountResult, error) {
	key := m.Type + ":" + m.Path + "@" + m.Version
	f.mounts = append(f.mounts, key)
	f.calls = append(f.calls, "mount:"+m.Path)
	// The version is part of the key, so a repeat of the same version is a
	// no-op and a version bump both changes and reloads -- as the real client does.
	if f.mountResult != nil {
		return *f.mountResult, nil
	}
	wrote := f.wrote("mount:" + key)
	return vault.MountResult{Changed: wrote, Reload: wrote}, nil
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
	f.calls = append(f.calls, "reload:"+name)
	return nil
}
func (f *fakeVault) EnsureRole(_ context.Context, r vault.Role) (bool, error) {
	key := r.Mount + "/" + r.RolesPath + "/" + r.Name
	f.ensuredRoles = append(f.ensuredRoles, key)
	f.calls = append(f.calls, "role:"+key)
	return f.wrote("role:" + key), nil
}
func (f *fakeVault) ListRoles(_ context.Context, mount, rolesPath string) ([]string, error) {
	return f.rolesByPath[mount+"/"+rolesPath], nil
}
func (f *fakeVault) DeleteRole(_ context.Context, mount, rolesPath, name string) error {
	f.deletedRoles = append(f.deletedRoles, mount+"/"+rolesPath+"/"+name)
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

// indexOf returns the position of want in s, or -1.
func indexOf(s []string, want string) int {
	for i, v := range s {
		if v == want {
			return i
		}
	}
	return -1
}

func TestReconcileRolesApplied(t *testing.T) {
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog: []config.CatalogEntry{{
			Name: "vault-plugin-secrets-foo", Type: config.PluginTypeSecret, Version: "0.3.1",
			Source: config.Source{URL: "https://x/foo.zip"},
		}},
		Mounts: []config.MountEntry{{
			Path: "foo", Plugin: "vault-plugin-secrets-foo", Type: config.PluginTypeSecret, Version: "0.3.1",
		}},
		Roles: []config.RoleEntry{
			{Mount: "foo", RolesPath: "roles", Name: "reader", Data: map[string]any{"ttl": "1h"}},
			{Mount: "foo", RolesPath: "roles", Name: "writer", Data: map[string]any{"ttl": "2h"}},
		},
	}
	pods := &fakePods{pods: []string{"vault-0"}}
	fv := &fakeVault{}
	r := New(fv, pods, fakeFetcher{}, testConfig())
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	// (a) declared roles are applied at the default <mount>/roles/<name> path.
	for _, want := range []string{"foo/roles/reader", "foo/roles/writer"} {
		if !has(fv.ensuredRoles, want) {
			t.Errorf("role %q not ensured; got %v", want, fv.ensuredRoles)
		}
	}
	// (b) role apply happens after mount reconcile + reload.
	roleIdx := indexOf(fv.calls, "role:foo/roles/reader")
	mountIdx := indexOf(fv.calls, "mount:foo")
	reloadIdx := indexOf(fv.calls, "reload:vault-plugin-secrets-foo")
	if roleIdx < 0 || mountIdx < 0 || reloadIdx < 0 {
		t.Fatalf("missing expected calls; got %v", fv.calls)
	}
	if !(mountIdx < roleIdx && reloadIdx < roleIdx) {
		t.Errorf("roles must apply after mount+reload; calls=%v", fv.calls)
	}
}

func TestReconcileRolesEmptyAppliesNothing(t *testing.T) {
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneNever},
		Mounts: []config.MountEntry{{
			Path: "foo", Plugin: "p", Type: config.PluginTypeSecret, Version: "0.3.1",
		}},
		Catalog: []config.CatalogEntry{{Name: "p", Type: config.PluginTypeSecret, Version: "0.3.1", Source: config.Source{URL: "https://x"}}},
	}
	pods := &fakePods{pods: []string{"vault-0"}}
	fv := &fakeVault{}
	r := New(fv, pods, fakeFetcher{}, testConfig())
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if len(fv.ensuredRoles) != 0 {
		t.Errorf("empty roles must ensure nothing; got %v", fv.ensuredRoles)
	}
}

func TestReconcileRolesPruneFull(t *testing.T) {
	// Managed mount "foo" has roles reader+writer live; spec declares only reader,
	// so writer must be deleted. Under PruneFull.
	fv := &fakeVault{
		managed:     []vault.ManagedMount{{Path: "foo", Type: "secret", Plugin: "p", Version: "1.0.0"}},
		rolesByPath: map[string][]string{"foo/roles": {"reader", "writer"}},
	}
	pods := &fakePods{pods: []string{"vault-0"}}
	r := New(fv, pods, fakeFetcher{}, testConfig())
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog:  []config.CatalogEntry{{Name: "p", Type: config.PluginTypeSecret, Version: "1.0.0", Source: config.Source{URL: "https://x"}}},
		Mounts:   []config.MountEntry{{Path: "foo", Plugin: "p", Type: config.PluginTypeSecret, Version: "1.0.0"}},
		Roles:    []config.RoleEntry{{Mount: "foo", RolesPath: "roles", Name: "reader"}},
	}
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	// (f) declared role kept, undesired one deleted under the declared rolesPath.
	if !has(fv.deletedRoles, "foo/roles/writer") {
		t.Errorf("undeclared role writer should be pruned; got %v", fv.deletedRoles)
	}
	if has(fv.deletedRoles, "foo/roles/reader") {
		t.Errorf("declared role reader must not be pruned; got %v", fv.deletedRoles)
	}
}

func TestReconcileRolesCustomRolesPath(t *testing.T) {
	// A role with a two-level, plugin-owned rolesPath writes/lists/deletes under
	// <mount>/realm/example/roles. The undeclared "old" role there is pruned; the
	// declared "reader" is kept.
	fv := &fakeVault{
		managed:     []vault.ManagedMount{{Path: "foo", Type: "secret", Plugin: "p", Version: "1.0.0"}},
		rolesByPath: map[string][]string{"foo/realm/example/roles": {"reader", "old"}},
	}
	pods := &fakePods{pods: []string{"vault-0"}}
	r := New(fv, pods, fakeFetcher{}, testConfig())
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog:  []config.CatalogEntry{{Name: "p", Type: config.PluginTypeSecret, Version: "1.0.0", Source: config.Source{URL: "https://x"}}},
		Mounts:   []config.MountEntry{{Path: "foo", Plugin: "p", Type: config.PluginTypeSecret, Version: "1.0.0"}},
		Roles:    []config.RoleEntry{{Mount: "foo", RolesPath: "realm/example/roles", Name: "reader"}},
	}
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if !has(fv.ensuredRoles, "foo/realm/example/roles/reader") {
		t.Errorf("role not ensured at custom rolesPath; got %v", fv.ensuredRoles)
	}
	if !has(fv.deletedRoles, "foo/realm/example/roles/old") {
		t.Errorf("undeclared role at custom rolesPath should be pruned; got %v", fv.deletedRoles)
	}
	if has(fv.deletedRoles, "foo/realm/example/roles/reader") {
		t.Errorf("declared role must not be pruned; got %v", fv.deletedRoles)
	}
}

func TestReconcileRolesUndeclaredRolesPathNotEnumerated(t *testing.T) {
	// Deliberate limitation: a rolesPath the ConfigMap never mentions (here the
	// classic "roles" path, since the spec only declares "realm/example/roles")
	// is not enumerated, so its stale roles are NOT pruned. vpm cannot discover a
	// plugin's rolesPaths it was not told about.
	fv := &fakeVault{
		managed: []vault.ManagedMount{{Path: "foo", Type: "secret", Plugin: "p", Version: "1.0.0"}},
		rolesByPath: map[string][]string{
			"foo/roles":               {"stale"},
			"foo/realm/example/roles": {"reader"},
		},
	}
	pods := &fakePods{pods: []string{"vault-0"}}
	r := New(fv, pods, fakeFetcher{}, testConfig())
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneFull},
		Catalog:  []config.CatalogEntry{{Name: "p", Type: config.PluginTypeSecret, Version: "1.0.0", Source: config.Source{URL: "https://x"}}},
		Mounts:   []config.MountEntry{{Path: "foo", Plugin: "p", Type: config.PluginTypeSecret, Version: "1.0.0"}},
		Roles:    []config.RoleEntry{{Mount: "foo", RolesPath: "realm/example/roles", Name: "reader"}},
	}
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if has(fv.deletedRoles, "foo/roles/stale") {
		t.Errorf("undeclared rolesPath must not be enumerated/pruned; got %v", fv.deletedRoles)
	}
	if len(fv.deletedRoles) != 0 {
		t.Errorf("nothing should be pruned; got %v", fv.deletedRoles)
	}
}

func TestReconcileRolesNoPruneUnderNonFull(t *testing.T) {
	// Under non-full prune modes, no role is deleted even if live roles are undeclared.
	for _, mode := range []config.PruneMode{config.PruneDeregister, config.PruneNever} {
		t.Run(string(mode), func(t *testing.T) {
			fv := &fakeVault{
				managed:     []vault.ManagedMount{{Path: "foo", Type: "secret", Plugin: "p", Version: "1.0.0"}},
				rolesByPath: map[string][]string{"foo/roles": {"reader", "writer"}},
			}
			pods := &fakePods{pods: []string{"vault-0"}}
			r := New(fv, pods, fakeFetcher{}, testConfig())
			spec := &config.Spec{
				Settings: config.Settings{PruneMode: mode},
				Catalog:  []config.CatalogEntry{{Name: "p", Type: config.PluginTypeSecret, Version: "1.0.0", Source: config.Source{URL: "https://x"}}},
				Mounts:   []config.MountEntry{{Path: "foo", Plugin: "p", Type: config.PluginTypeSecret, Version: "1.0.0"}},
				Roles:    []config.RoleEntry{{Mount: "foo", RolesPath: "roles", Name: "reader"}},
			}
			if err := r.Reconcile(context.Background(), spec); err != nil {
				t.Fatal(err)
			}
			if len(fv.deletedRoles) != 0 {
				t.Errorf("mode %s must not prune roles; got %v", mode, fv.deletedRoles)
			}
		})
	}
}

// A reload tears down and re-initializes the backend on every HA node, so only a
// version move triggers one. A description or options edit is a real change --
// logged and counted -- that must leave running instances alone.
func TestReconcileReloadsOnlyOnVersionMoves(t *testing.T) {
	fv := &fakeVault{mountResult: &vault.MountResult{Changed: true, Reload: false}}
	r := New(fv, &fakePods{pods: []string{"vault-0"}}, fakeFetcher{}, testConfig())
	spec := &config.Spec{
		Settings: config.Settings{PruneMode: config.PruneNever},
		Mounts: []config.MountEntry{{
			Path: "foo", Plugin: "p", Type: config.PluginTypeSecret, Version: "1.0.0",
		}},
	}
	if err := r.Reconcile(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if len(fv.reloaded) != 0 {
		t.Errorf("a mount change that needs no reload triggered one: %v", fv.reloaded)
	}
	if len(fv.mounts) != 1 {
		t.Errorf("mount not reconciled; got %v", fv.mounts)
	}
}
