package reconcile

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/vault"
)

// ecBase declares the two secret mounts the engine config tests write to.
const ecBase = `
catalog:
  - name: p
    type: secret
    version: "1.0.0"
    source: {url: https://x/p}
mounts:
  - {path: github, plugin: p, type: secret, version: "1.0.0"}
  - {path: other, plugin: p, type: secret, version: "1.0.0"}
`

// ecGithub is the shared entry: one data field and one Secret-sourced field.
const ecGithub = `engineConfigs:
  - mount: github
    data: {app_id: 4627856}
    secretRef: {name: creds, keys: {prv_key: pem}}
`

// ecGithubAndOther adds a second entry reading the same Secret.
const ecGithubAndOther = ecGithub + `  - mount: other
    secretRef: {name: creds, keys: {prv_key: pem}}
`

func ecSpec(t *testing.T, mode config.PruneMode, extra string) *config.Spec {
	t.Helper()
	s, err := config.Parse([]byte("settings:\n  pruneMode: " + string(mode) + "\n" + ecBase + extra))
	if err != nil {
		t.Fatalf("parsing spec: %v", err)
	}
	return s
}

// ecRig bundles the fakes an engine config test drives.
type ecRig struct {
	fv      *fakeVault
	secrets *fakeSecrets
	r       *Reconciler
}

func newECRig(creds map[string][]byte) *ecRig {
	g := &ecRig{secrets: &fakeSecrets{secrets: map[string]map[string][]byte{}}}
	if creds != nil {
		g.secrets.secrets["creds"] = creds
	}
	g.restart()
	return g
}

// restart replaces the Vault fake and the reconciler while keeping the state
// Secret and the live mount UUIDs: a manager restart against the same Vault.
func (g *ecRig) restart() {
	uuids := map[string]string{"github": "u-1", "other": "u-o"}
	if g.fv != nil {
		uuids = g.fv.uuids
	}
	g.fv = &fakeVault{uuids: uuids}
	g.r = New(g.fv, &fakePods{pods: []string{"vault-0"}}, g.secrets, fakeFetcher{}, testConfig())
}

func (g *ecRig) pass(spec *config.Spec) error { return g.r.Reconcile(context.Background(), spec) }

func (g *ecRig) mustPass(t *testing.T, spec *config.Spec) {
	t.Helper()
	if err := g.pass(spec); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// records decodes the state Secret the way the next pass will read it.
func (g *ecRig) records(t *testing.T) map[string]engineConfigRecord {
	t.Helper()
	data, ok := g.secrets.secrets[testConfig().StateSecretName]
	if !ok {
		t.Fatal("state Secret was never saved")
	}
	var out map[string]engineConfigRecord
	if err := json.Unmarshal(data[engineConfigStateKey], &out); err != nil {
		t.Fatalf("decoding state: %v", err)
	}
	return out
}

// observeLogs swaps the reconciler's logger for one that records every entry.
func (g *ecRig) observeLogs() *observer.ObservedLogs {
	core, logs := observer.New(zapcore.DebugLevel)
	g.r.log = zap.New(core).Sugar().With("component", "reconcile")
	return logs
}

func count(s []string, want string) int {
	n := 0
	for _, v := range s {
		if v == want {
			n++
		}
	}
	return n
}

func TestEngineConfigFirstPassWritesAndRecords(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	before := snapshotCounters(t)
	g.mustPass(t, ecSpec(t, config.PruneNever, ecGithub))
	after := snapshotCounters(t)

	if !reflect.DeepEqual(g.fv.configWrites, []string{"github/config"}) {
		t.Fatalf("writes = %v, want [github/config]", g.fv.configWrites)
	}
	want := map[string]any{"app_id": 4627856, "prv_key": "KEY1"}
	if got := g.fv.configBodies["github/config"]; !reflect.DeepEqual(got, want) {
		t.Errorf("body = %#v, want %#v", got, want)
	}
	rec, ok := g.records(t)["github/config"]
	if !ok {
		t.Fatal("no record for github/config")
	}
	if rec.MountUUID != "u-1" {
		t.Errorf("mountUUID = %q, want u-1", rec.MountUUID)
	}
	if b, err := hex.DecodeString(rec.SHA256); err != nil || len(b) != 32 {
		t.Errorf("sha256 = %q, want 64 hex chars", rec.SHA256)
	}
	if got := after.engineConf - before.engineConf; got != 1 {
		t.Errorf("engine_config: recorded %v, want 1", got)
	}
}

func TestEngineConfigSecondPassWritesNothing(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	spec := ecSpec(t, config.PruneNever, ecGithub)
	g.mustPass(t, spec)
	g.mustPass(t, spec)
	if len(g.fv.configWrites) != 1 {
		t.Errorf("writes = %v, want one", g.fv.configWrites)
	}
}

func TestEngineConfigRotatedSecretRewrites(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	spec := ecSpec(t, config.PruneNever, ecGithub)
	g.mustPass(t, spec)
	g.secrets.secrets["creds"] = map[string][]byte{"pem": []byte("KEY2")}
	g.mustPass(t, spec)

	if len(g.fv.configWrites) != 2 {
		t.Fatalf("writes = %v, want two", g.fv.configWrites)
	}
	if got := g.fv.configBodies["github/config"]["prv_key"]; got != "KEY2" {
		t.Errorf("second body prv_key = %v, want KEY2", got)
	}
}

func TestEngineConfigRestartWithPersistedStateWritesNothing(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	spec := ecSpec(t, config.PruneNever, ecGithub)
	g.mustPass(t, spec)
	g.restart()
	g.mustPass(t, spec)
	if len(g.fv.configWrites) != 0 {
		t.Errorf("restarted manager wrote %v, want nothing", g.fv.configWrites)
	}
}

func TestEngineConfigRecreatedMountRewrites(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	spec := ecSpec(t, config.PruneNever, ecGithub)
	g.mustPass(t, spec)
	g.fv.uuids["github"] = "u-2"
	g.mustPass(t, spec)

	if len(g.fv.configWrites) != 2 {
		t.Fatalf("writes = %v, want two", g.fv.configWrites)
	}
	if got := g.records(t)["github/config"].MountUUID; got != "u-2" {
		t.Errorf("mountUUID = %q, want u-2", got)
	}
}

func TestEngineConfigMovedPathRewrites(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	g.mustPass(t, ecSpec(t, config.PruneNever, ecGithub))
	g.mustPass(t, ecSpec(t, config.PruneNever, strings.Replace(ecGithub, "  - mount: github\n", "  - mount: github\n    path: setup\n", 1)))

	if !has(g.fv.configWrites, "github/setup") {
		t.Errorf("writes = %v, want one to github/setup", g.fv.configWrites)
	}
	recs := g.records(t)
	if _, ok := recs["github/config"]; ok {
		t.Error("record for the old path was kept")
	}
	if _, ok := recs["github/setup"]; !ok {
		t.Error("no record for the new path")
	}
}

func TestEngineConfigMissingSecretFailsBeforeAnyWrite(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	spec := ecSpec(t, config.PruneNever, `roles:
  - {mount: github, name: r}
`+ecGithub+`  - mount: other
    secretRef: {name: missing, keys: {prv_key: pem}}
`)
	err := g.pass(spec)
	if err == nil {
		t.Fatal("expected an error for a missing Secret")
	}
	if !strings.Contains(err.Error(), "vault/missing") {
		t.Errorf("error %q does not name the Secret", err)
	}
	if len(g.fv.configWrites) != 0 {
		t.Errorf("writes = %v, want none", g.fv.configWrites)
	}
	for _, c := range g.fv.calls {
		if strings.HasPrefix(c, "role:") {
			t.Errorf("roles ran after a failed engine config step: %v", g.fv.calls)
			break
		}
	}
}

func TestEngineConfigMissingKeyFails(t *testing.T) {
	g := newECRig(map[string][]byte{"other": []byte("x")})
	err := g.pass(ecSpec(t, config.PruneNever, ecGithub))
	if err == nil {
		t.Fatal("expected an error for a missing key")
	}
	if !strings.Contains(err.Error(), "creds") || !strings.Contains(err.Error(), "pem") {
		t.Errorf("error %q does not name the Secret and key", err)
	}
	if len(g.fv.configWrites) != 0 {
		t.Errorf("writes = %v, want none", g.fv.configWrites)
	}
}

func TestEngineConfigRejectsEmptySecretValue(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("")})
	if err := g.pass(ecSpec(t, config.PruneNever, ecGithub)); err == nil {
		t.Fatal("expected an error for an empty value")
	}
	if len(g.fv.configWrites) != 0 {
		t.Errorf("writes = %v, want none", g.fv.configWrites)
	}
}

func TestEngineConfigRejectsNonUTF8SecretValue(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": {0xff, 0xfe}})
	err := g.pass(ecSpec(t, config.PruneNever, ecGithub))
	if err == nil {
		t.Fatal("expected an error for a non UTF-8 value")
	}
	if !strings.Contains(err.Error(), "creds") || !strings.Contains(err.Error(), "pem") {
		t.Errorf("error %q does not name the Secret and key", err)
	}
	if len(g.fv.configWrites) != 0 {
		t.Errorf("writes = %v, want none", g.fv.configWrites)
	}
}

func TestEngineConfigSecretValueIsWrittenVerbatim(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY\n")})
	g.mustPass(t, ecSpec(t, config.PruneNever, ecGithub))
	if got := g.fv.configBodies["github/config"]["prv_key"]; got != "KEY\n" {
		t.Errorf("prv_key = %q, want %q", got, "KEY\n")
	}
}

func TestEngineConfigDigestStableAcrossParsesAndKeyOrder(t *testing.T) {
	first := ecSpec(t, config.PruneNever, `engineConfigs:
  - mount: github
    data: {opts: {x: 1, y: [a, {z: 2}]}, scopes: [a, b], app_id: 7}
`)
	second := ecSpec(t, config.PruneNever, `engineConfigs:
  - mount: github
    data: {app_id: 7, scopes: [a, b], opts: {y: [a, {z: 2}], x: 1}}
`)
	d1, err := engineConfigDigest("github/config", first.EngineConfigs[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := engineConfigDigest("github/config", second.EngineConfigs[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Errorf("digests differ across key order: %s vs %s", d1, d2)
	}

	g := newECRig(nil)
	g.mustPass(t, first)
	g.mustPass(t, second)
	writes := len(g.fv.configWrites)
	g.restart()
	g.mustPass(t, second)
	if total := writes + len(g.fv.configWrites); total != 1 {
		t.Errorf("wrote %d times across three passes, want 1", total)
	}
}

func TestEngineConfigSharedSecretIsReadOncePerPass(t *testing.T) {
	g := newECRig(nil)
	n := 0
	g.secrets.onGet = func(name string) map[string][]byte {
		if name != "creds" {
			return nil
		}
		n++
		return map[string][]byte{"pem": []byte("KEY" + strings.Repeat("x", n))}
	}
	g.mustPass(t, ecSpec(t, config.PruneNever, ecGithubAndOther))

	if got := g.secrets.gets["creds"]; got != 1 {
		t.Errorf("creds fetched %d times, want 1", got)
	}
	a, b := g.fv.configBodies["github/config"]["prv_key"], g.fv.configBodies["other/config"]["prv_key"]
	if a == nil || a != b {
		t.Errorf("bodies disagree on the shared value: %v vs %v", a, b)
	}
}

func TestEngineConfigErrorsAreScrubbedOfSecretValues(t *testing.T) {
	cases := map[string]struct {
		pem, tok string
		errText  string
		want     string   // must appear in the result
		absent   []string // must not appear in the result
	}{
		"plain": {
			pem: "KEY1", tok: "OTHER", errText: `invalid key "KEY1"`,
			want: "<redacted>", absent: []string{"KEY1"},
		},
		"json escaped": {
			pem: `a"b`, tok: "zzz", errText: `bad value a\"b`,
			want: "<redacted>", absent: []string{`a\"b`, `a"b`},
		},
		"overlapping values": {
			pem: "abc", tok: "cdef", errText: "xabcdefy",
			want: "x<redacted>y", absent: []string{"def", "ef"},
		},
		"one value contains the other": {
			pem: "abc", tok: "abcdef", errText: "abcdef",
			want: "<redacted>", absent: []string{"def"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			g := newECRig(map[string][]byte{"pem": []byte(tc.pem), "tok": []byte(tc.tok)})
			g.fv.writeErr = map[string]error{"github/config": errors.New(tc.errText)}
			err := g.pass(ecSpec(t, config.PruneNever, `engineConfigs:
  - mount: github
    secretRef: {name: creds, keys: {prv_key: pem, token: tok}}
`))
			if err == nil {
				t.Fatal("expected the write error")
			}
			got := err.Error()
			if !strings.Contains(got, tc.want) {
				t.Errorf("error %q lacks %q", got, tc.want)
			}
			for _, s := range tc.absent {
				if strings.Contains(got, s) {
					t.Errorf("error %q leaks %q", got, s)
				}
			}
			if errors.Unwrap(err) != nil {
				t.Error("scrubbed error unwraps to the raw error")
			}
		})
	}
}

func TestScrubSecrets(t *testing.T) {
	cases := map[string]struct {
		values []string
		in     string
		want   string
	}{
		"overlapping values":           {values: []string{"abc", "cdef"}, in: "xabcdefy", want: "x<redacted>y"},
		"one value contains the other": {values: []string{"abc", "abcdef"}, in: "abcdef", want: "<redacted>"},
		"json escaped":                 {values: []string{`a"b`}, in: `v=a\"b.`, want: "v=<redacted>."},
		"separate runs":                {values: []string{"k"}, in: "k-k", want: "<redacted>-<redacted>"},
		"value overlaps itself":        {values: []string{"aba"}, in: "ababa", want: "<redacted>"},
		"no secret in the error":       {values: []string{"KEY1"}, in: "vault: HTTP 400", want: "vault: HTTP 400"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := scrubSecrets(errors.New(tc.in), tc.values)
			if got.Error() != tc.want {
				t.Errorf("scrubSecrets(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	if scrubSecrets(nil, []string{"x"}) != nil {
		t.Error("scrubSecrets(nil) is not nil")
	}
}

func TestEngineConfigCheckpointsWhenALaterWriteFails(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	g.fv.writeErr = map[string]error{"other/config": errors.New("HTTP 500")}
	spec := ecSpec(t, config.PruneNever, ecGithubAndOther)

	if err := g.pass(spec); err == nil {
		t.Fatal("expected the write error on pass one")
	}
	recs := g.records(t)
	if _, ok := recs["github/config"]; !ok {
		t.Error("the landed github/config write was not checkpointed")
	}
	if _, ok := recs["other/config"]; ok {
		t.Error("a failed write was recorded")
	}
	if err := g.pass(spec); err == nil {
		t.Fatal("expected the write error on pass two")
	}
	if got := count(g.fv.configWrites, "github/config"); got != 1 {
		t.Errorf("github/config written %d times, want 1", got)
	}
}

func TestEngineConfigStateFailsClosed(t *testing.T) {
	state := testConfig().StateSecretName
	cases := map[string]func(s *fakeSecrets){
		"read error":       func(s *fakeSecrets) { s.getErr = map[string]error{state: errors.New("forbidden")} },
		"missing key":      func(s *fakeSecrets) { s.secrets[state] = map[string][]byte{"other": []byte("{}")} },
		"not json":         func(s *fakeSecrets) { s.secrets[state] = map[string][]byte{engineConfigStateKey: []byte("not json")} },
		"null document":    func(s *fakeSecrets) { s.secrets[state] = map[string][]byte{engineConfigStateKey: []byte("null")} },
		"array document":   func(s *fakeSecrets) { s.secrets[state] = map[string][]byte{engineConfigStateKey: []byte("[]")} },
		"empty sha256":     func(s *fakeSecrets) { s.secrets[state] = stateDoc(`{"github/config":{"sha256":"","mountUUID":"u-1"}}`) },
		"empty mount uuid": func(s *fakeSecrets) { s.secrets[state] = stateDoc(`{"github/config":{"sha256":"ab","mountUUID":""}}`) },
		"null record":      func(s *fakeSecrets) { s.secrets[state] = stateDoc(`{"github/config":null}`) },
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
			seed(g.secrets)
			err := g.pass(ecSpec(t, config.PruneNever, ecGithub))
			if err == nil {
				t.Fatal("expected the pass to fail closed")
			}
			if !strings.Contains(err.Error(), state) {
				t.Errorf("error %q does not name the state Secret", err)
			}
			if len(g.fv.configWrites) != 0 {
				t.Errorf("writes = %v, want none", g.fv.configWrites)
			}
			if len(g.secrets.puts) != 0 {
				t.Errorf("state saved %v, want no save", g.secrets.puts)
			}
		})
	}

	t.Run("empty object is valid empty state", func(t *testing.T) {
		g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
		g.secrets.secrets[state] = stateDoc(`{}`)
		g.mustPass(t, ecSpec(t, config.PruneNever, ecGithub))
		if len(g.fv.configWrites) != 1 {
			t.Errorf("writes = %v, want one", g.fv.configWrites)
		}
		if len(g.secrets.puts) != 1 || g.secrets.puts[0].rv == "" {
			t.Errorf("puts = %v, want one update carrying the resourceVersion", g.secrets.puts)
		}
	})
}

func stateDoc(doc string) map[string][]byte {
	return map[string][]byte{engineConfigStateKey: []byte(doc)}
}

func TestEngineConfigMountUUIDsValidatedBeforeAnyWrite(t *testing.T) {
	cases := map[string]func(u map[string]string){
		"mount absent":     func(u map[string]string) { delete(u, "other") },
		"empty mount uuid": func(u map[string]string) { u["other"] = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
			// A stale record that a completed step would drop, so reaching the
			// save would show up as a put.
			g.secrets.secrets[testConfig().StateSecretName] = stateDoc(`{"gone/config":{"sha256":"ab","mountUUID":"u-g"}}`)
			mutate(g.fv.uuids)
			err := g.pass(ecSpec(t, config.PruneNever, ecGithubAndOther))
			if err == nil {
				t.Fatal("expected an error for the unusable mount UUID")
			}
			if !strings.Contains(err.Error(), "other") {
				t.Errorf("error %q does not name the mount", err)
			}
			if len(g.fv.configWrites) != 0 {
				t.Errorf("writes = %v, want none", g.fv.configWrites)
			}
			if len(g.secrets.puts) != 0 {
				t.Errorf("state saved %v, want no save", g.secrets.puts)
			}
		})
	}
}

func TestEngineConfigStateSaveConflictFailsThePass(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	g.secrets.secrets[testConfig().StateSecretName] = stateDoc(`{}`)
	g.secrets.putErr = errors.New("conflict: the object has been modified")
	spec := ecSpec(t, config.PruneNever, ecGithub)

	for i := 1; i <= 2; i++ {
		if err := g.pass(spec); err == nil {
			t.Fatalf("pass %d: expected the save error", i)
		}
		if len(g.fv.configWrites) != i {
			t.Fatalf("pass %d: writes = %v, want %d", i, g.fv.configWrites, i)
		}
	}
	g.secrets.putErr = nil
	g.mustPass(t, spec)
	if len(g.fv.configWrites) != 3 {
		t.Fatalf("pass 3: writes = %v, want 3", g.fv.configWrites)
	}
	if _, ok := g.records(t)["github/config"]; !ok {
		t.Fatal("pass 3 did not save the record")
	}
	g.mustPass(t, spec)
	if len(g.fv.configWrites) != 3 {
		t.Errorf("pass 4: writes = %v, want still 3", g.fv.configWrites)
	}
}

func TestEngineConfigRemovedEntryDropsRecordOnly(t *testing.T) {
	for _, mode := range []config.PruneMode{config.PruneFull, config.PruneDeregister, config.PruneNever} {
		t.Run(string(mode), func(t *testing.T) {
			g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
			g.fv.managed = []vault.ManagedMount{{Path: "github", Type: "secret", Plugin: "p", Version: "1.0.0"}}
			g.mustPass(t, ecSpec(t, mode, ecGithub))
			mark := len(g.fv.calls)

			g.mustPass(t, ecSpec(t, mode, ""))
			for _, c := range g.fv.calls[mark:] {
				if strings.HasPrefix(c, "config:") {
					t.Errorf("removed entry caused %s", c)
				}
			}
			if len(g.fv.disabled)+len(g.fv.deletedRoles)+len(g.fv.deregistered) != 0 {
				t.Errorf("removed entry deleted something: disabled=%v roles=%v deregistered=%v",
					g.fv.disabled, g.fv.deletedRoles, g.fv.deregistered)
			}
			if _, ok := g.records(t)["github/config"]; ok {
				t.Error("record for the removed entry was kept")
			}

			g.mustPass(t, ecSpec(t, mode, ecGithub))
			if len(g.fv.configWrites) != 2 {
				t.Errorf("writes = %v, want the re-added entry written again", g.fv.configWrites)
			}
		})
	}
}

func TestEngineConfigRunsAfterReloadAndBeforeRoles(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	g.mustPass(t, ecSpec(t, config.PruneNever, `roles:
  - {mount: github, name: r}
`+ecGithub))

	cfg := indexOf(g.fv.calls, "config:github/config")
	if cfg < 0 {
		t.Fatalf("no engine config write; calls=%v", g.fv.calls)
	}
	sawReload, sawRole := false, false
	for i, c := range g.fv.calls {
		switch {
		case strings.HasPrefix(c, "reload:"):
			sawReload = true
			if i > cfg {
				t.Errorf("%s ran after the engine config write; calls=%v", c, g.fv.calls)
			}
		case strings.HasPrefix(c, "role:"):
			sawRole = true
			if i < cfg {
				t.Errorf("%s ran before the engine config write; calls=%v", c, g.fv.calls)
			}
		}
	}
	if !sawReload || !sawRole {
		t.Fatalf("expected both a reload and a role; calls=%v", g.fv.calls)
	}
}

func TestEngineConfigNoEntriesNoStateSecretIsNoop(t *testing.T) {
	g := newECRig(nil)
	g.mustPass(t, ecSpec(t, config.PruneNever, ""))
	if len(g.secrets.puts) != 0 {
		t.Errorf("puts = %v, want none", g.secrets.puts)
	}
	if got := g.secrets.gets[testConfig().StateSecretName]; got != 1 {
		t.Errorf("state Secret read %d times, want once per pass", got)
	}
}

func TestEngineConfigNoEntriesUnreadableStateSkips(t *testing.T) {
	state := testConfig().StateSecretName
	cases := map[string]func(s *fakeSecrets){
		"read error": func(s *fakeSecrets) {
			s.getErr = map[string]error{state: errors.New(`secrets "vpm-state" is forbidden: cannot get resource "secrets"`)}
		},
		"not json": func(s *fakeSecrets) { s.secrets[state] = stateDoc("not json") },
	}
	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			g := newECRig(nil)
			seed(g.secrets)
			logs := g.observeLogs()
			if err := g.pass(ecSpec(t, config.PruneNever, "roles:\n  - {mount: github, name: r}\n")); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if len(g.secrets.puts) != 0 {
				t.Errorf("puts = %v, want none", g.secrets.puts)
			}
			ranRole := false
			for _, c := range g.fv.calls {
				ranRole = ranRole || strings.HasPrefix(c, "role:")
			}
			if !ranRole {
				t.Errorf("roles did not run; calls=%v", g.fv.calls)
			}
			warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
			if len(warns) != 1 {
				t.Fatalf("logged %d warnings, want 1", len(warns))
			}
			if fields := warns[0].ContextMap(); fields["secret"] != "vault/"+state || fields["component"] != "reconcile" {
				t.Errorf("warning fields = %v, want secret vault/%s and component reconcile", fields, state)
			}
		})
	}
}

func TestEngineConfigCreatesStateBeforeFirstWrite(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	g.secrets.oplog = &g.fv.calls
	state := testConfig().StateSecretName
	g.mustPass(t, ecSpec(t, config.PruneNever, ecGithub))

	create, write := indexOf(g.fv.calls, "put:"+state+"@"), indexOf(g.fv.calls, "config:github/config")
	if create < 0 || write < 0 || create > write {
		t.Fatalf("want the state Secret created before the write; calls=%v", g.fv.calls)
	}
	want := []fakePut{{name: state, rv: ""}, {name: state, rv: "1"}}
	if !reflect.DeepEqual(g.secrets.puts, want) {
		t.Errorf("puts = %v, want a create then an update carrying its resourceVersion", g.secrets.puts)
	}
	if _, ok := g.records(t)["github/config"]; !ok {
		t.Error("no record for github/config")
	}
}

func TestEngineConfigPersistentCreateFailureWritesNothing(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	g.secrets.putErr = errors.New(`secrets is forbidden: cannot create resource "secrets"`)
	spec := ecSpec(t, config.PruneNever, ecGithub)
	for i := 1; i <= 3; i++ {
		if err := g.pass(spec); err == nil {
			t.Fatalf("pass %d: expected the create error", i)
		}
	}
	for _, c := range g.fv.calls {
		if strings.HasPrefix(c, "config:") {
			t.Fatalf("wrote Vault without a state Secret; calls=%v", g.fv.calls)
		}
	}
}

func TestEngineConfigLogsWriteReason(t *testing.T) {
	g := newECRig(map[string][]byte{"pem": []byte("KEY1")})
	logs := g.observeLogs()
	spec := ecSpec(t, config.PruneNever, ecGithub)

	g.mustPass(t, spec)
	g.fv.uuids["github"] = "u-2"
	g.mustPass(t, spec)
	g.secrets.secrets["creds"] = map[string][]byte{"pem": []byte("KEY2")}
	g.mustPass(t, spec)

	var got []string
	for _, e := range logs.FilterMessage("wrote engine config").All() {
		f := e.ContextMap()
		if f["mount"] != "github" || f["path"] != "config" {
			t.Errorf("fields = %v, want mount github and path config", f)
		}
		got = append(got, f["reason"].(string))
	}
	want := []string{"unrecorded", "mount_recreated", "changed"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reasons = %v, want %v", got, want)
	}
}
