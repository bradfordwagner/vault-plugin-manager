package vault

import (
	"testing"
	"time"

	"github.com/hashicorp/vault/api"
)

func TestParsePluginType(t *testing.T) {
	ok := map[string]api.PluginType{
		"secret":   api.PluginTypeSecrets,
		"auth":     api.PluginTypeCredential,
		"database": api.PluginTypeDatabase,
	}
	for in, want := range ok {
		got, err := parsePluginType(in)
		if err != nil {
			t.Errorf("parsePluginType(%q) errored: %v", in, err)
		}
		if got != want {
			t.Errorf("parsePluginType(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"", "unknown", "secrets", "bogus"} {
		if _, err := parsePluginType(bad); err == nil {
			t.Errorf("parsePluginType(%q) expected error, got nil", bad)
		}
	}
}

func TestWithManagedAndIsManaged(t *testing.T) {
	got := withManaged(map[string]string{"foo": "bar"})
	if got["foo"] != "bar" {
		t.Errorf("withManaged dropped existing option: %v", got)
	}
	if !isManaged(got) {
		t.Errorf("withManaged output not detected as managed: %v", got)
	}
	if isManaged(map[string]string{"foo": "bar"}) {
		t.Errorf("unmarked options wrongly detected as managed")
	}
	if isManaged(nil) {
		t.Errorf("nil options wrongly detected as managed")
	}
}

func TestSameVersion(t *testing.T) {
	equal := [][2]string{
		{"1.0.0", "v1.0.0"},
		{"v1.0.0", "1.0.0"},
		{"1.0.0", "1.0.0"},
		{"v2.3.4", "v2.3.4"},
	}
	for _, p := range equal {
		if !sameVersion(p[0], p[1]) {
			t.Errorf("sameVersion(%q,%q) = false, want true", p[0], p[1])
		}
	}
	if sameVersion("1.0.0", "1.0.1") {
		t.Errorf("sameVersion(1.0.0,1.0.1) = true, want false")
	}
}

func TestNormPath(t *testing.T) {
	for in, want := range map[string]string{
		"foo":   "foo",
		"/foo":  "foo",
		"foo/":  "foo",
		"/foo/": "foo",
		"a/b/":  "a/b",
	} {
		if got := normPath(in); got != want {
			t.Errorf("normPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	const max = 15 * time.Second
	cases := []struct{ cur, want time.Duration }{
		{time.Second, 2 * time.Second},
		{4 * time.Second, 8 * time.Second},
		{8 * time.Second, max},  // 16s would exceed the cap
		{max, max},              // already at the cap
		{30 * time.Second, max}, // above the cap
		{1 << 62, max},          // doubling overflows to <=0
	}
	for _, c := range cases {
		if got := nextBackoff(c.cur, max); got != c.want {
			t.Errorf("nextBackoff(%v, %v) = %v, want %v", c.cur, max, got, c.want)
		}
	}
}

func TestRenewalLead(t *testing.T) {
	if got := renewalLead(100 * time.Second); got != 90*time.Second {
		t.Errorf("renewalLead(100s) = %v, want 90s", got)
	}
	if got := renewalLead(0); got != time.Second {
		t.Errorf("renewalLead(0) = %v, want 1s", got)
	}
	if got := renewalLead(500 * time.Millisecond); got != time.Second {
		t.Errorf("renewalLead(500ms) = %v, want 1s floor", got)
	}
}

// Description and options drift must produce a tune, or the change log reports
// an edit that is never applied: config.Diff emits `description` and `options`
// for mounts, and before this both were only sent on the initial enable.
func TestMountTune(t *testing.T) {
	managed := map[string]string{managedByKey: managedByValue}

	t.Run("converged mount does not tune", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Description: "github creds", Options: map[string]string{"a": "1"}}
		opts := map[string]string{managedByKey: managedByValue, "a": "1"}
		if _, drift := mountTune("v1.0.0", "github creds", opts, m); drift {
			t.Error("an unchanged mount must not be tuned")
		}
	})

	t.Run("version bump", func(t *testing.T) {
		m := Mount{Version: "1.1.0"}
		tune, drift := mountTune("v1.0.0", "", managed, m)
		if !drift || tune.PluginVersion == nil || *tune.PluginVersion != "1.1.0" {
			t.Errorf("want a version tune, got drift=%v tune=%+v", drift, tune)
		}
	})

	t.Run("description edit", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Description: "new words"}
		tune, drift := mountTune("v1.0.0", "old words", managed, m)
		if !drift || tune.Description == nil || *tune.Description != "new words" {
			t.Errorf("want a description tune, got drift=%v tune=%+v", drift, tune)
		}
		if tune.PluginVersion != nil {
			t.Error("an unchanged version must not be sent")
		}
	})

	t.Run("options edit keeps what Vault maintains", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Options: map[string]string{"a": "2"}}
		existing := map[string]string{managedByKey: managedByValue, "a": "1", "vault_owned": "keep"}
		tune, drift := mountTune("v1.0.0", "", existing, m)
		if !drift || tune.Options == nil {
			t.Fatalf("want an options tune, got drift=%v tune=%+v", drift, tune)
		}
		got := *tune.Options
		if got["a"] != "2" {
			t.Errorf("declared option not applied: %v", got)
		}
		if got["vault_owned"] != "keep" {
			t.Errorf("a tune must not drop options it did not declare: %v", got)
		}
		if !isManaged(got) {
			t.Errorf("a tune must not drop the ownership marker: %v", got)
		}
	})

	// Adopting a mount somebody else created would make it prunable the moment
	// its path left the ConfigMap, so the marker is never added by a tune.
	t.Run("an unmanaged mount is not adopted", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Options: map[string]string{"a": "2"}}
		tune, drift := mountTune("v1.0.0", "", map[string]string{"a": "1"}, m)
		if !drift || tune.Options == nil {
			t.Fatalf("want an options tune, got drift=%v tune=%+v", drift, tune)
		}
		if isManaged(*tune.Options) {
			t.Errorf("tune marked a mount this manager did not create: %v", *tune.Options)
		}
	})
}
