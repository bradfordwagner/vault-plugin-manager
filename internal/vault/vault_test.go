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
