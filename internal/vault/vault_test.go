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
	managedOpts := func(extra map[string]string) map[string]string {
		out := map[string]string{managedByKey: managedByValue}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}

	t.Run("converged mount does not tune", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Description: "github creds", Options: map[string]string{"a": "1"}}
		live := liveMount{Version: "v1.0.0", Description: "github creds", Options: managedOpts(map[string]string{"a": "1"})}
		if _, res := mountTune(live, m); res.Changed {
			t.Error("an unchanged mount must not be tuned")
		}
	})

	t.Run("a mount with no declared config stays quiet", func(t *testing.T) {
		m := Mount{Version: "1.0.0"}
		live := liveMount{Version: "v1.0.0", Options: managedOpts(nil)}
		if _, res := mountTune(live, m); res.Changed {
			t.Error("a mount declaring no description or options must not be tuned")
		}
	})

	t.Run("version bump reloads", func(t *testing.T) {
		m := Mount{Version: "1.1.0"}
		live := liveMount{Version: "v1.0.0", Options: managedOpts(nil)}
		tune, res := mountTune(live, m)
		if !res.Changed || tune.PluginVersion == nil || *tune.PluginVersion != "1.1.0" {
			t.Errorf("want a version tune, got %+v / %+v", res, tune)
		}
		if !res.Reload {
			t.Error("a version move must reload: the binary behind the mount changed")
		}
	})

	// A reload tears down and re-initializes the backend on every HA node, so a
	// metadata edit must not cause one.
	t.Run("description edit does not reload", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Description: "new words"}
		live := liveMount{Version: "v1.0.0", Description: "old words", Options: managedOpts(nil)}
		tune, res := mountTune(live, m)
		if !res.Changed || tune.Description == nil || *tune.Description != "new words" {
			t.Errorf("want a description tune, got %+v / %+v", res, tune)
		}
		if res.Reload {
			t.Error("a description edit must not reload the plugin")
		}
		if tune.PluginVersion != nil {
			t.Error("an unchanged version must not be sent")
		}
	})

	// Options DO need a reload: Vault hands them to the backend as its config at
	// initialization, so a tune alone persists them without putting them in
	// effect.
	t.Run("options edit reloads", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Options: map[string]string{"tier": "silver"}}
		live := liveMount{Version: "v1.0.0", Options: managedOpts(map[string]string{"tier": "gold"})}
		tune, res := mountTune(live, m)
		if !res.Changed || tune.Options == nil {
			t.Fatalf("want an options tune, got %+v / %+v", res, tune)
		}
		if (*tune.Options)["tier"] != "silver" || !isManaged(*tune.Options) {
			t.Errorf("tuned options wrong: %v", *tune.Options)
		}
		if !res.Reload {
			t.Error("an options edit must reload: the backend read them at init")
		}
	})

	// An option that LEAVES the spec must be removed, and a removal is expressed
	// as an EMPTY VALUE: Vault's tune merges the map it is given into the stored
	// one and deletes only the keys whose value is empty, so simply omitting the
	// key would leave it in place -- tuning, and reloading, on every pass forever.
	t.Run("removed option is tuned away", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Options: map[string]string{"a": "1"}}
		live := liveMount{Version: "v1.0.0", Options: managedOpts(map[string]string{"a": "1", "tier": "gold"})}
		tune, res := mountTune(live, m)
		if !res.Changed || tune.Options == nil {
			t.Fatalf("want an options tune, got %+v / %+v", res, tune)
		}
		if got, ok := (*tune.Options)["tier"]; !ok || got != "" {
			t.Errorf("a removed option must be sent as an empty value, got %q (present=%v): %v", got, ok, *tune.Options)
		}
		if (*tune.Options)["a"] != "1" || !isManaged(*tune.Options) {
			t.Errorf("removal dropped more than it should: %v", *tune.Options)
		}
	})

	// ...and once Vault has dropped it, the mount is converged: an absent key and
	// an empty value read the same, so the tune does not repeat (which would also
	// reload the plugin on every pass, since options drift reloads).
	t.Run("a settled removal does not tune again", func(t *testing.T) {
		m := Mount{Version: "1.0.0", Options: map[string]string{"a": "1"}}
		for _, live := range []liveMount{
			{Version: "v1.0.0", Options: managedOpts(map[string]string{"a": "1"})},             // Vault deleted the key
			{Version: "v1.0.0", Options: managedOpts(map[string]string{"a": "1", "tier": ""})}, // ...or emptied it
		} {
			if _, res := mountTune(live, m); res.Changed {
				t.Errorf("a settled removal tuned again (options %v)", live.Options)
			}
		}
	})

	// The same removal, seen by a process that never wrote this mount: a restart
	// must not resurrect the option a memory-based approach would have lost.
	t.Run("removed option is tuned away after a restart", func(t *testing.T) {
		m := Mount{Version: "1.0.0"}
		live := liveMount{Version: "v1.0.0", Options: managedOpts(map[string]string{"tier": "gold"})}
		tune, res := mountTune(live, m)
		if !res.Changed || tune.Options == nil {
			t.Fatalf("want an options tune, got %+v / %+v", res, tune)
		}
		if got, ok := (*tune.Options)["tier"]; !ok || got != "" {
			t.Errorf("a fresh process must still clear the stale option, got %q (present=%v)", got, ok)
		}
		if !isManaged(*tune.Options) {
			t.Errorf("the ownership marker must survive: %v", *tune.Options)
		}
	})

	// A mount this manager did not create is not ours to rewrite: the prune pass
	// refuses to delete it, so tuning must not restyle it either. Its version is
	// still pinned, which is what the spec is really declaring.
	t.Run("a foreign mount keeps its description and options", func(t *testing.T) {
		m := Mount{Version: "1.1.0", Description: "ours", Options: map[string]string{"a": "2"}}
		live := liveMount{Version: "v1.0.0", Description: "theirs", Options: map[string]string{"a": "1"}}
		tune, res := mountTune(live, m)
		if !res.Changed || tune.PluginVersion == nil {
			t.Fatalf("want the version pinned, got %+v / %+v", res, tune)
		}
		if tune.Description != nil {
			t.Errorf("tune rewrote a foreign mount's description: %q", *tune.Description)
		}
		if tune.Options != nil {
			t.Errorf("tune rewrote a foreign mount's options: %v", *tune.Options)
		}
	})
}
