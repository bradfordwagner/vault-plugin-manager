package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/vault/api"
)

// num builds a value the way Vault's API decodes JSON numbers (UseNumber), so
// these cases exercise the real read-back type rather than a Go int.
func num(s string) json.Number { return json.Number(s) }

func TestRoleUpToDateNormalizesVaultReadBack(t *testing.T) {
	cases := []struct {
		name     string
		existing map[string]any
		desired  map[string]any
		want     bool
	}{{
		name:     "ttl written as a duration reads back as seconds",
		existing: map[string]any{"ttl": num("300"), "max_ttl": num("3600")},
		desired:  map[string]any{"ttl": "5m", "max_ttl": "1h"},
		want:     true,
	}, {
		name:     "omitted fields are plugin defaults, not drift",
		existing: map[string]any{"ttl": num("300"), "sync_upstream": false, "realm_roles": []any{}},
		desired:  map[string]any{"ttl": "5m"},
		want:     true,
	}, {
		name:     "a real ttl change is drift",
		existing: map[string]any{"ttl": num("300")},
		desired:  map[string]any{"ttl": "10m"},
		want:     false,
	}, {
		name:     "two spellings of one duration",
		existing: map[string]any{"ttl": "60m"},
		desired:  map[string]any{"ttl": "1h"},
		want:     true,
	}, {
		name:     "bare numeric string is seconds",
		existing: map[string]any{"ttl": num("300")},
		desired:  map[string]any{"ttl": "300"},
		want:     true,
	}, {
		name:     "int from the ConfigMap against json.Number from Vault",
		existing: map[string]any{"ttl": num("300")},
		desired:  map[string]any{"ttl": 300},
		want:     true,
	}, {
		name:     "string list read back as []any",
		existing: map[string]any{"realm_roles": []any{"a", "b"}},
		desired:  map[string]any{"realm_roles": []string{"a", "b"}},
		want:     true,
	}, {
		name:     "a reordered list is the same set, not drift",
		existing: map[string]any{"realm_roles": []any{"admin", "viewer"}},
		desired:  map[string]any{"realm_roles": []string{"viewer", "admin"}},
		want:     true,
	}, {
		name:     "a declared empty list read back as null is not drift",
		existing: map[string]any{"realm_roles": nil},
		desired:  map[string]any{"realm_roles": []string{}},
		want:     true,
	}, {
		name:     "duplicates are counted, not collapsed",
		existing: map[string]any{"realm_roles": []any{"a", "a"}},
		desired:  map[string]any{"realm_roles": []string{"a", "b"}},
		want:     false,
	}, {
		name:     "changed list member is drift",
		existing: map[string]any{"realm_roles": []any{"a", "b"}},
		desired:  map[string]any{"realm_roles": []string{"a", "c"}},
		want:     false,
	}, {
		name:     "dropped list member is drift",
		existing: map[string]any{"realm_roles": []any{"a", "b"}},
		desired:  map[string]any{"realm_roles": []string{"a"}},
		want:     false,
	}, {
		name:     "declared field missing from the live role is drift",
		existing: map[string]any{"ttl": num("300")},
		desired:  map[string]any{"ttl": "5m", "source_client_id": "argocd"},
		want:     false,
	}, {
		name:     "strings and bools compare plainly",
		existing: map[string]any{"source_client_id": "argocd", "sync_upstream": true},
		desired:  map[string]any{"source_client_id": "argocd", "sync_upstream": true},
		want:     true,
	}, {
		name:     "flipped bool is drift",
		existing: map[string]any{"sync_upstream": true},
		desired:  map[string]any{"sync_upstream": false},
		want:     false,
	}, {
		name:     "nested object matches key for key",
		existing: map[string]any{"opts": map[string]any{"a": num("1"), "b": "x"}},
		desired:  map[string]any{"opts": map[string]any{"a": 1, "b": "x"}},
		want:     true,
	}, {
		name:     "extra key in a nested object is drift",
		existing: map[string]any{"opts": map[string]any{"a": num("1"), "b": "x"}},
		desired:  map[string]any{"opts": map[string]any{"a": 1}},
		want:     false,
	}, {
		name:     "an empty spec body never differs",
		existing: map[string]any{"ttl": num("300")},
		desired:  map[string]any{},
		want:     true,
	}, {
		name:     "nil against a set value is drift",
		existing: map[string]any{"ttl": nil},
		desired:  map[string]any{"ttl": "5m"},
		want:     false,
	}, {
		name:     "a non-duration string is not read as a number",
		existing: map[string]any{"realm": num("5")},
		desired:  map[string]any{"realm": "example"},
		want:     false,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleUpToDate(tc.existing, tc.desired); got != tc.want {
				t.Errorf("roleUpToDate = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAsSeconds(t *testing.T) {
	ok := map[string]float64{"5m": 300, "1h": 3600, "300": 300, "0": 0, "90s": 90}
	for in, want := range ok {
		got, valid := asSeconds(in)
		if !valid || got != want {
			t.Errorf("asSeconds(%q) = (%v,%v), want (%v,true)", in, got, valid, want)
		}
	}
	for _, bad := range []any{"example", "", 300, nil, []string{"5m"}} {
		if _, valid := asSeconds(bad); valid {
			t.Errorf("asSeconds(%v) reported a duration", bad)
		}
	}
}

// roleServer is a Vault stand-in for one role path. It answers reads with the
// body it holds and MERGES writes into it, the way a plugin whose role carries
// defaults does: a key the spec stops declaring keeps its old value on read-back.
type roleServer struct {
	body   map[string]any
	writes int
}

func (rs *roleServer) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if rs.body == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": rs.body})
		case http.MethodPut, http.MethodPost:
			var in map[string]any
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				t.Errorf("decoding role write: %v", err)
			}
			if rs.body == nil {
				rs.body = map[string]any{}
			}
			for k, v := range in {
				rs.body[k] = v
			}
			rs.writes++
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(srv.Close)

	api, err := api.NewClient(&api.Config{Address: srv.URL})
	if err != nil {
		t.Fatalf("building api client: %v", err)
	}
	return &Client{api: api}
}

func TestEnsureRoleWritesOnceThenGoesQuiet(t *testing.T) {
	rs := &roleServer{}
	c := rs.start(t)
	role := Role{Mount: "keycloak", RolesPath: "roles", Name: "reader", Data: map[string]any{"ttl": "5m"}}

	changed, err := c.EnsureRole(context.Background(), role)
	if err != nil || !changed {
		t.Fatalf("first EnsureRole = (%v,%v), want (true,nil)", changed, err)
	}
	// Vault normalizes the TTL to seconds on read-back; that must not read as drift.
	rs.body["ttl"] = json.Number("300")

	for i := 0; i < 3; i++ {
		changed, err := c.EnsureRole(context.Background(), role)
		if err != nil {
			t.Fatalf("repeat EnsureRole: %v", err)
		}
		if changed {
			t.Fatalf("repeat %d reported a write against an unchanged role", i)
		}
	}
	if rs.writes != 1 {
		t.Errorf("wrote %d times, want 1", rs.writes)
	}
}

// Dropping a key from the spec must still be written, even though every key that
// REMAINS matches what Vault holds. Otherwise the removal is silent and
// permanent: the change log reports the key moved, and nothing applies it.
func TestEnsureRoleWritesWhenADeclaredKeyIsRemoved(t *testing.T) {
	rs := &roleServer{}
	c := rs.start(t)
	full := Role{Mount: "keycloak", RolesPath: "roles", Name: "reader",
		Data: map[string]any{"ttl": "5m", "max_ttl": "1h"}}

	if _, err := c.EnsureRole(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	rs.body["ttl"], rs.body["max_ttl"] = json.Number("300"), json.Number("3600")
	if _, err := c.EnsureRole(context.Background(), full); err != nil {
		t.Fatal(err)
	}
	if rs.writes != 1 {
		t.Fatalf("converged run wrote %d times, want 1", rs.writes)
	}

	// max_ttl leaves the spec. The live role still reports it (a plugin default
	// now), so a declared-keys-only comparison would call this converged.
	reduced := full
	reduced.Data = map[string]any{"ttl": "5m"}
	changed, err := c.EnsureRole(context.Background(), reduced)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || rs.writes != 2 {
		t.Errorf("removal not applied: changed=%v writes=%d, want true/2", changed, rs.writes)
	}

	// ...and it goes quiet again on the reduced spec.
	if changed, _ := c.EnsureRole(context.Background(), reduced); changed {
		t.Errorf("reduced spec kept writing after it was applied")
	}
}

// A fresh process has no record of what it wrote, so it writes each role once.
// That is what makes an edit made while the manager was DOWN converge.
func TestEnsureRoleWritesOnFirstSightingAfterRestart(t *testing.T) {
	rs := &roleServer{body: map[string]any{"ttl": json.Number("300")}}
	role := Role{Mount: "keycloak", RolesPath: "roles", Name: "reader", Data: map[string]any{"ttl": "5m"}}

	c := rs.start(t)
	changed, err := c.EnsureRole(context.Background(), role)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("first sighting of a role in a new process must be written")
	}
}
