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
