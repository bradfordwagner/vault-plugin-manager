package config

import (
	"strings"
	"testing"
)

// mustParse keeps the tests reading like the ConfigMap an operator edits.
func mustParse(t *testing.T, raw string) *Spec {
	t.Helper()
	s, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return s
}

const baseSpec = `
settings:
  pruneMode: full
  resyncInterval: 5m
catalog:
  - name: foo
    type: secret
    version: "1.0.0"
    source:
      url: https://example.com/foo.tar.gz
mounts:
  - path: foo
    plugin: foo
    type: secret
    version: "1.0.0"
roles:
  - mount: foo
    name: reader
    data:
      ttl: "1h"
`

// lines renders a diff the way the Runner logs it, one change per line.
func lines(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.String())
	}
	return out
}

func TestDiffNoChange(t *testing.T) {
	spec := mustParse(t, baseSpec)
	if got := Diff(spec, mustParse(t, baseSpec)); len(got) != 0 {
		t.Fatalf("want no changes, got %v", lines(got))
	}
}

func TestDiffFromNilReportsEveryEntry(t *testing.T) {
	got := lines(Diff(nil, mustParse(t, baseSpec)))
	want := []string{
		"catalog foo@1.0.0 added: type=secret, url=https://example.com/foo.tar.gz",
		"mounts secret:foo added: plugin=foo, version=1.0.0",
		"roles foo/roles/reader added: data keys: ttl",
	}
	assertLines(t, got, want)
}

func TestDiffVersionBumpReadsAsRemoveAndAdd(t *testing.T) {
	old := mustParse(t, baseSpec)
	new := mustParse(t, strings.ReplaceAll(baseSpec, `"1.0.0"`, `"1.1.0"`))

	got := lines(Diff(old, new))
	want := []string{
		"catalog foo@1.0.0 removed: type=secret",
		"catalog foo@1.1.0 added: type=secret, url=https://example.com/foo.tar.gz",
		"mounts secret:foo changed: version 1.0.0 -> 1.1.0",
	}
	assertLines(t, got, want)
}

func TestDiffSettingsAndRoleData(t *testing.T) {
	old := mustParse(t, baseSpec)
	new := mustParse(t, strings.NewReplacer(
		"pruneMode: full", "pruneMode: never",
		`ttl: "1h"`, `ttl: "2h"`,
	).Replace(baseSpec))

	got := lines(Diff(old, new))
	want := []string{
		"settings pruneMode changed: full -> never",
		"roles foo/roles/reader changed: data keys: ttl",
	}
	assertLines(t, got, want)
}

func TestDiffSourceChangeOnSameVersion(t *testing.T) {
	old := mustParse(t, baseSpec)
	new := mustParse(t, strings.Replace(baseSpec,
		"      url: https://example.com/foo.tar.gz",
		"      image: registry:5000/foo:1\n      path: /plugin/foo", 1))

	got := lines(Diff(old, new))
	want := []string{
		"catalog foo@1.0.0 changed: source url=https://example.com/foo.tar.gz -> image=registry:5000/foo:1 path=/plugin/foo",
	}
	assertLines(t, got, want)
}

func TestDiffRemovalsFlagPruneCandidates(t *testing.T) {
	old := mustParse(t, baseSpec)
	new := mustParse(t, `
settings:
  pruneMode: full
catalog: []
mounts: []
roles: []
`)

	got := lines(Diff(old, new))
	want := []string{
		"catalog foo@1.0.0 removed: type=secret",
		"mounts secret:foo removed: plugin=foo, version=1.0.0 (prune candidate)",
		"roles foo/roles/reader removed: prune candidate",
	}
	assertLines(t, got, want)
}

// A leading "v" on a version must not read as a change: Vault reports versions
// back with it, and the reconciler compares them trimmed.
func TestDiffIgnoresLeadingVOnMountVersion(t *testing.T) {
	old := mustParse(t, baseSpec)
	new := mustParse(t, strings.Replace(baseSpec,
		"  - path: foo\n    plugin: foo\n    type: secret\n    version: \"1.0.0\"",
		"  - path: foo\n    plugin: foo\n    type: secret\n    version: \"v1.0.0\"", 1))

	for _, c := range Diff(old, new) {
		if c.Section == SectionMounts {
			t.Fatalf("unexpected mount change: %s", c)
		}
	}
}

func assertLines(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change[%d]:\n got: %s\nwant: %s", i, got[i], want[i])
		}
	}
}

const engineConfigDiffBase = `
catalog:
  - name: p
    type: secret
    version: "1"
    source: {url: https://x}
mounts:
  - path: github
    plugin: p
    type: secret
    version: "1"
`

func TestDiffEngineConfigsAdded(t *testing.T) {
	s := mustParse(t, engineConfigDiffBase+`
engineConfigs:
  - mount: github
    data: {app_id: 4627856}
    secretRef: {name: creds, keys: {prv_key: pem}}
`)
	var got []Change
	for _, c := range Diff(nil, s) {
		if c.Section == SectionEngineConfigs {
			got = append(got, c)
		}
	}
	if len(got) != 1 {
		t.Fatalf("got %d engineConfigs changes, want 1: %v", len(got), got)
	}
	c := got[0]
	if c.Key != "github/config" || c.Action != ActionAdded {
		t.Errorf("got key=%q action=%q", c.Key, c.Action)
	}
	for _, want := range []string{"app_id", "creds", "prv_key"} {
		if !strings.Contains(c.Detail, want) {
			t.Errorf("detail %q missing %q", c.Detail, want)
		}
	}
	if strings.Contains(c.Detail, "4627856") {
		t.Errorf("detail %q leaks a data value", c.Detail)
	}
}

func TestDiffEngineConfigsChangedReportsKeysNotValues(t *testing.T) {
	mk := func(appID, secret, field string) *Spec {
		return mustParse(t, engineConfigDiffBase+"engineConfigs:\n  - mount: github\n    data: {app_id: "+appID+"}\n"+
			"    secretRef: {name: "+secret+", keys: {prv_key: "+field+"}}\n")
	}
	detail := func(old, new *Spec) string {
		t.Helper()
		got := Diff(old, new)
		if len(got) != 1 || got[0].Section != SectionEngineConfigs || got[0].Action != ActionChanged {
			t.Fatalf("got %v, want one engineConfigs changed", got)
		}
		return got[0].Detail
	}

	d := detail(mk("1", "a", "pem"), mk("2", "a", "pem"))
	if !strings.Contains(d, "data keys: app_id") || strings.ContainsAny(d, "12") {
		t.Errorf("detail %q", d)
	}
	if d := detail(mk("1", "a", "pem"), mk("1", "b", "pem")); !strings.Contains(d, "secretRef name a -> b") {
		t.Errorf("detail %q", d)
	}
	if d := detail(mk("1", "a", "pem"), mk("1", "a", "key")); !strings.Contains(d, "secretRef fields: prv_key") {
		t.Errorf("detail %q", d)
	}
}

func TestDiffEngineConfigsAddedSecretRefOnlyOmitsDataKeys(t *testing.T) {
	s := mustParse(t, engineConfigDiffBase+"engineConfigs:\n  - mount: github\n    secretRef: {name: creds, keys: {prv_key: pem}}\n")
	for _, c := range Diff(nil, s) {
		if c.Section != SectionEngineConfigs {
			continue
		}
		if strings.Contains(c.Detail, "data keys") {
			t.Errorf("detail %q mentions data keys for a secretRef-only entry", c.Detail)
		}
		if want := "secretRef creds fields: prv_key"; c.Detail != want {
			t.Errorf("detail %q, want %q", c.Detail, want)
		}
		return
	}
	t.Fatal("no engineConfigs change")
}

func TestDiffEngineConfigsSecretRefAddedAndRemoved(t *testing.T) {
	const withRef = "    secretRef: {name: creds, keys: {prv_key: pem}}\n"
	plain := mustParse(t, engineConfigDiffBase+"engineConfigs:\n  - mount: github\n    data: {a: 1}\n")
	ref := mustParse(t, engineConfigDiffBase+"engineConfigs:\n  - mount: github\n    data: {a: 1}\n"+withRef)
	cases := []struct {
		name     string
		old, new *Spec
		want     string
	}{
		{"added", plain, ref, "secretRef added: creds fields: prv_key"},
		{"removed", ref, plain, "secretRef removed: creds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Diff(tc.old, tc.new)
			if len(got) != 1 || got[0].Section != SectionEngineConfigs || got[0].Action != ActionChanged {
				t.Fatalf("got %v, want one engineConfigs changed", got)
			}
			if got[0].Detail != tc.want {
				t.Errorf("detail %q, want %q", got[0].Detail, tc.want)
			}
		})
	}
}

func TestDiffEngineConfigsRemoved(t *testing.T) {
	old := mustParse(t, engineConfigDiffBase+"engineConfigs:\n  - mount: github\n    data: {a: 1}\n")
	got := Diff(old, mustParse(t, engineConfigDiffBase))
	want := []Change{{SectionEngineConfigs, "github/config", ActionRemoved, "config left in Vault"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDiffEngineConfigsOrderedAfterMountsBeforeRoles(t *testing.T) {
	s := mustParse(t, engineConfigDiffBase+`
roles:
  - mount: github
    name: r
engineConfigs:
  - mount: github
    data: {a: 1}
`)
	var sections []string
	for _, c := range Diff(nil, s) {
		sections = append(sections, c.Section)
	}
	want := []string{SectionCatalog, SectionMounts, SectionEngineConfigs, SectionRoles}
	if strings.Join(sections, ",") != strings.Join(want, ",") {
		t.Errorf("sections = %v, want %v", sections, want)
	}
}
