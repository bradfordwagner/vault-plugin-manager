package vault

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
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
//
// A REMOVED key is written even though the keys that remain still match, and so
// is the first sighting of a role in this process. Only declared keys can be
// compared (see roleUpToDate), so dropping "max_ttl" from the spec otherwise
// looks identical to never having declared it, and the old value would live in
// Vault forever -- silently, since the change log reports the key as moved. The
// first-sighting write is what makes a removal that happened while the manager
// was DOWN converge too: there is no record to compare that edit against.
func (c *Client) EnsureRole(ctx context.Context, r Role) (changed bool, err error) {
	path := normPath(r.Mount) + "/" + r.RolesPath + "/" + r.Name

	if c.sameRoleKeysAsLastWrite(path, r.Data) {
		if existing, ok := c.readRole(ctx, path); ok && roleUpToDate(existing, r.Data) {
			return false, nil
		}
	}
	if _, err := c.api.Logical().WriteWithContext(ctx, path, r.Data); err != nil {
		return false, fmt.Errorf("vault: writing role %s: %w", path, err)
	}
	c.recordRoleKeys(path, r.Data)
	return true, nil
}

// sameRoleKeysAsLastWrite reports whether this process has already written path
// with exactly this set of keys, which is the precondition for trusting a
// declared-keys-only comparison.
func (c *Client) sameRoleKeysAsLastWrite(path string, data map[string]any) bool {
	c.roleMu.Lock()
	defer c.roleMu.Unlock()
	written, ok := c.roleKeys[path]
	return ok && written == roleKeySet(data)
}

func (c *Client) recordRoleKeys(path string, data map[string]any) {
	c.roleMu.Lock()
	defer c.roleMu.Unlock()
	if c.roleKeys == nil {
		c.roleKeys = make(map[string]string)
	}
	c.roleKeys[path] = roleKeySet(data)
}

// roleKeySet renders a role body's key names as one comparable string.
func roleKeySet(data map[string]any) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
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
	// A declared empty list and a plugin that reports the field as null are the
	// same state; treating them as different rewrites the role on every pass.
	if got == nil || want == nil {
		return emptyList(got) && emptyList(want)
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

	// Lists compare as multisets. Vault secret engines commonly store list-ish
	// fields as sets and read them back sorted or deduplicated, so an index-by-
	// index comparison would report drift on every pass for a plugin that
	// reorders -- rewriting the role forever, which is the exact failure this
	// comparison exists to prevent. The cost is that a pure REORDER of a list
	// whose plugin does treat order as meaningful is not detected; declare such
	// a field's change by also changing a value if that ever matters.
	if wl, ok := asSlice(want); ok {
		gl, ok := asSlice(got)
		if !ok || len(gl) != len(wl) {
			return false
		}
		used := make([]bool, len(gl))
		for _, w := range wl {
			matched := false
			for i, g := range gl {
				if used[i] || !sameRoleValue(g, w) {
					continue
				}
				used[i], matched = true, true
				break
			}
			if !matched {
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

// emptyList reports whether v is nil or an empty list, the two ways "no values"
// crosses the write/read-back boundary.
func emptyList(v any) bool {
	if v == nil {
		return true
	}
	l, ok := asSlice(v)
	return ok && len(l) == 0
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
