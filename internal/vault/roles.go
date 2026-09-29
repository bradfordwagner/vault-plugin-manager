package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// Role is a secret-engine role written to <Mount>/<RolesPath>/<Name>. Data is
// written verbatim to the plugin, which owns the schema. RolesPath is the
// already-defaulted, slash-trimmed subpath between the mount and the role name
// (e.g. "roles" for the classic layout, or "realm/example/roles").
type Role struct {
	Mount     string
	RolesPath string
	Name      string
	Data      map[string]any
}

// EnsureRole writes the role body at <mount>/<rolesPath>/<name> when it is
// missing or differs from what Vault holds, and reports whether it wrote. The
// read-compare-write shape matches EnsurePlugin/EnsureMount: an unconditional
// write is idempotent for the plugin but still a real Vault call and a real
// audit-log entry on every reconcile, and it leaves the role_upsert metric
// permanently non-zero.
func (c *Client) EnsureRole(ctx context.Context, r Role) (changed bool, err error) {
	path := normPath(r.Mount) + "/" + r.RolesPath + "/" + r.Name
	if existing, ok := c.readRole(ctx, path); ok && roleUpToDate(existing, r.Data) {
		return false, nil
	}
	if _, err := c.api.Logical().WriteWithContext(ctx, path, r.Data); err != nil {
		return false, fmt.Errorf("vault: writing role %s: %w", path, err)
	}
	return true, nil
}

// readRole returns the live role body. ok is false when the role is absent, the
// read fails, or the plugin does not serve a readable role — all of which mean
// "cannot compare", so the caller writes, which is exactly what this client did
// before the comparison existed. A read that fails must never fail a reconcile.
func (c *Client) readRole(ctx context.Context, path string) (map[string]any, bool) {
	secret, err := c.api.Logical().ReadWithContext(ctx, path)
	if err != nil || secret == nil || secret.Data == nil {
		return nil, false
	}
	return secret.Data, true
}

// roleUpToDate reports whether every key the spec declares already holds the
// declared value in Vault.
//
// ONLY declared keys are compared. A role read back carries the plugin's
// defaults for every field the spec omits, so comparing the whole body would
// report "changed" forever — which would just trade one always-on counter for
// another. The plugin owns the role schema, so this stays generic: vpm cannot
// know which absent fields are defaults and which are meaningful.
func roleUpToDate(existing, desired map[string]any) bool {
	for k, want := range desired {
		got, ok := existing[k]
		if !ok || !sameRoleValue(got, want) {
			return false
		}
	}
	return true
}

// sameRoleValue compares one role field across the write/read-back boundary,
// where Vault normalizes values: a TTL written as "5m" reads back as the number
// 300, and numbers decode as json.Number rather than int. Comparing with
// reflect.DeepEqual instead makes every such field differ on every pass.
func sameRoleValue(got, want any) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}

	// Numbers, including a duration string read back as seconds.
	if wn, ok := asNumber(want); ok {
		if gn, ok := asNumber(got); ok {
			return gn == wn
		}
		if gs, ok := asSeconds(got); ok {
			return gs == wn
		}
		return false
	}
	if ws, ok := asSeconds(want); ok {
		if gn, ok := asNumber(got); ok {
			return gn == ws
		}
		if gs, ok := asSeconds(got); ok {
			return gs == ws // different spellings of one duration ("60m" / "1h")
		}
		return false
	}

	// Lists: order-sensitive, because the plugin may treat order as meaningful.
	if wl, ok := asSlice(want); ok {
		gl, ok := asSlice(got)
		if !ok || len(gl) != len(wl) {
			return false
		}
		for i := range wl {
			if !sameRoleValue(gl[i], wl[i]) {
				return false
			}
		}
		return true
	}

	// Nested objects: every declared key must match, and no extra live key may
	// appear — an object is written whole, so it has no plugin defaults to skip.
	if wm, ok := want.(map[string]any); ok {
		gm, ok := got.(map[string]any)
		if !ok || len(gm) != len(wm) {
			return false
		}
		return roleUpToDate(gm, wm)
	}

	gt, wt := reflect.TypeOf(got), reflect.TypeOf(want)
	if gt != wt || !gt.Comparable() {
		return false
	}
	return got == want
}

// asNumber reports v as a float64 if it is numeric. Vault's API decodes JSON
// with UseNumber, so an integer field arrives as json.Number, while the parsed
// ConfigMap yields int/float64.
func asNumber(v any) (float64, bool) {
	if n, ok := v.(json.Number); ok {
		f, err := n.Float64()
		return f, err == nil
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(rv.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(rv.Uint()), true
	case reflect.Float32, reflect.Float64:
		return rv.Float(), true
	default:
		return 0, false
	}
}

// asSeconds reports a duration-ish string as seconds: "5m" (Go duration syntax,
// which is also Vault's) or a bare "300", which Vault reads as seconds.
func asSeconds(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d.Seconds(), true
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f, true
	}
	return 0, false
}

// asSlice normalizes any slice/array to []any so a []string from the ConfigMap
// compares against the []any Vault reads back.
func asSlice(v any) ([]any, bool) {
	rv := reflect.ValueOf(v)
	if k := rv.Kind(); k != reflect.Slice && k != reflect.Array {
		return nil, false
	}
	out := make([]any, rv.Len())
	for i := range out {
		out[i] = rv.Index(i).Interface()
	}
	return out, true
}

// ListRoles returns the role names registered under <mount>/<rolesPath>. A path
// that does not exist (404) or an empty listing yields an empty slice.
func (c *Client) ListRoles(ctx context.Context, mount, rolesPath string) ([]string, error) {
	path := normPath(mount) + "/" + rolesPath
	secret, err := c.api.Logical().ListWithContext(ctx, path)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("vault: listing roles at %s: %w", path, err)
	}
	if secret == nil || secret.Data == nil {
		return nil, nil
	}
	raw, ok := secret.Data["keys"].([]any)
	if !ok {
		return nil, nil
	}
	names := make([]string, 0, len(raw))
	for _, k := range raw {
		if s, ok := k.(string); ok {
			names = append(names, s)
		}
	}
	return names, nil
}

// DeleteRole removes the role at <mount>/<rolesPath>/<name>. A missing role is
// treated as success (idempotent).
func (c *Client) DeleteRole(ctx context.Context, mount, rolesPath, name string) error {
	path := normPath(mount) + "/" + rolesPath + "/" + name
	if _, err := c.api.Logical().DeleteWithContext(ctx, path); err != nil && !isNotFound(err) {
		return fmt.Errorf("vault: deleting role %s: %w", path, err)
	}
	return nil
}
