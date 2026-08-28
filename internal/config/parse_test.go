package config

import (
	"testing"
	"time"
)

func TestParseSettingsDefaults(t *testing.T) {
	// No settings block -> defaults applied.
	s, err := Parse([]byte("catalog: []\nmounts: []\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Settings.PruneMode != DefaultPruneMode {
		t.Errorf("pruneMode = %q, want %q", s.Settings.PruneMode, DefaultPruneMode)
	}
	if s.Settings.ResyncInterval.Duration() != DefaultResyncInterval {
		t.Errorf("resyncInterval = %v, want %v", s.Settings.ResyncInterval.Duration(), DefaultResyncInterval)
	}
	if s.Settings.LogLevel != DefaultLogLevel {
		t.Errorf("logLevel = %q, want %q", s.Settings.LogLevel, DefaultLogLevel)
	}
}

func TestParseSettingsExplicit(t *testing.T) {
	s, err := Parse([]byte(`
settings:
  pruneMode: deregister
  resyncInterval: 90s
  logLevel: debug
catalog: []
mounts: []
`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.Settings.PruneMode != PruneDeregister {
		t.Errorf("pruneMode = %q", s.Settings.PruneMode)
	}
	if s.Settings.ResyncInterval.Duration() != 90*time.Second {
		t.Errorf("resyncInterval = %v", s.Settings.ResyncInterval.Duration())
	}
	if s.Settings.LogLevel != "debug" {
		t.Errorf("logLevel = %q", s.Settings.LogLevel)
	}
}

func TestParseSettingsInvalid(t *testing.T) {
	cases := map[string]string{
		"bad prune mode": "settings: {pruneMode: sometimes}\ncatalog: []\n",
		"bad log level":  "settings: {logLevel: loud}\ncatalog: []\n",
		"bad duration":   "settings: {resyncInterval: fortnight}\ncatalog: []\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatalf("expected error for %q, got nil", name)
			}
		})
	}
}

func TestParseValid(t *testing.T) {
	raw := []byte(`
catalog:
  - name: vault-plugin-secrets-foo
    type: secret
    version: "0.3.1"
    source:
      url: https://example.com/foo_0.3.1.zip
      sha256: deadbeef
  - name: vault-plugin-secrets-bar
    type: secret
    version: "1.0.0"
    source:
      image: ghcr.io/org/bar:1.0.0
      path: /plugin/bar
mounts:
  - path: foo
    plugin: vault-plugin-secrets-foo
    type: secret
    version: "0.3.1"
`)
	s, err := Parse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.Catalog) != 2 || len(s.Mounts) != 1 {
		t.Fatalf("got catalog=%d mounts=%d", len(s.Catalog), len(s.Mounts))
	}
	if s.Mounts[0].Plugin != "vault-plugin-secrets-foo" {
		t.Fatalf("mount plugin ref = %q", s.Mounts[0].Plugin)
	}
}

func TestParseValidRoles(t *testing.T) {
	raw := []byte(`
catalog:
  - name: vault-plugin-secrets-foo
    type: secret
    version: "0.3.1"
    source:
      url: https://example.com/foo.zip
mounts:
  - path: foo
    plugin: vault-plugin-secrets-foo
    type: secret
    version: "0.3.1"
roles:
  - mount: foo
    name: reader
    data:
      ttl: "1h"
      policies:
        - default
        - read
  - mount: /foo/
    name: writer
  - mount: foo
    name: realm-reader
    rolesPath: /realm/example/roles/
`)
	s, err := Parse(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.Roles) != 3 {
		t.Fatalf("got roles=%d, want 3", len(s.Roles))
	}
	r0 := s.Roles[0]
	if r0.Mount != "foo" || r0.Name != "reader" {
		t.Fatalf("role[0] = %+v", r0)
	}
	// Omitted rolesPath defaults to the classic "roles" segment.
	if r0.RolesPath != DefaultRolesPath {
		t.Errorf("role[0].rolesPath = %q, want %q", r0.RolesPath, DefaultRolesPath)
	}
	// Nested data decodes to a map keyed by string.
	if got, ok := r0.Data["ttl"].(string); !ok || got != "1h" {
		t.Errorf("role[0].data[ttl] = %v (%T), want \"1h\"", r0.Data["ttl"], r0.Data["ttl"])
	}
	if _, ok := r0.Data["policies"].([]any); !ok {
		t.Errorf("role[0].data[policies] = %v (%T), want []any", r0.Data["policies"], r0.Data["policies"])
	}
	// A role.mount with surrounding slashes still resolves to the declared mount.
	if s.Roles[1].Name != "writer" {
		t.Errorf("role[1].name = %q", s.Roles[1].Name)
	}
	if s.Roles[1].RolesPath != DefaultRolesPath {
		t.Errorf("role[1].rolesPath = %q, want %q", s.Roles[1].RolesPath, DefaultRolesPath)
	}
	// An explicit rolesPath is trimmed of surrounding slashes and kept verbatim.
	if s.Roles[2].RolesPath != "realm/example/roles" {
		t.Errorf("role[2].rolesPath = %q, want %q", s.Roles[2].RolesPath, "realm/example/roles")
	}
}

func TestParseInvalidRoles(t *testing.T) {
	base := `
catalog:
  - name: p
    type: secret
    version: "1"
    source: {url: https://x}
mounts:
  - path: foo
    plugin: p
    type: secret
    version: "1"
`
	cases := map[string]string{
		"missing mount":           base + "roles:\n  - name: reader\n",
		"missing name":            base + "roles:\n  - mount: foo\n",
		"unknown mount":           base + "roles:\n  - mount: bar\n    name: reader\n",
		"name config":             base + "roles:\n  - mount: foo\n    name: config\n",
		"name with slash":         base + "roles:\n  - mount: foo\n    name: a/b\n",
		"rolesPath empty segment": base + "roles:\n  - mount: foo\n    name: reader\n    rolesPath: a//b\n",
		"rolesPath dot segment":   base + "roles:\n  - mount: foo\n    name: reader\n    rolesPath: a/./b\n",
		"rolesPath dotdot":        base + "roles:\n  - mount: foo\n    name: reader\n    rolesPath: a/../b\n",
		"rolesPath only slashes":  base + "roles:\n  - mount: foo\n    name: reader\n    rolesPath: /\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatalf("expected error for %q, got nil", name)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	cases := map[string]string{
		"missing source": `
catalog:
  - name: p
    type: secret
    version: "1"
    source: {}
`,
		"both url and image": `
catalog:
  - name: p
    type: secret
    version: "1"
    source: {url: https://x, image: ghcr.io/x:1, path: /p}
`,
		"image without path": `
catalog:
  - name: p
    type: secret
    version: "1"
    source: {image: ghcr.io/x:1}
`,
		"bad type": `
catalog:
  - name: p
    type: bogus
    version: "1"
    source: {url: https://x}
`,
		"mount references unknown plugin": `
catalog:
  - name: p
    type: secret
    version: "1"
    source: {url: https://x}
mounts:
  - path: m
    plugin: other
    type: secret
    version: "1"
`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw)); err == nil {
				t.Fatalf("expected error for %q, got nil", name)
			}
		})
	}
}
