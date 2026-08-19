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
