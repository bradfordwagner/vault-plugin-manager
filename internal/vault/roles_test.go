package vault

import (
	"encoding/json"
	"testing"
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
