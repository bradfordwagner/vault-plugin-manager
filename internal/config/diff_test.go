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
