package vault

import (
	"context"
	"fmt"
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

// EnsureRole upserts the role body at <mount>/<rolesPath>/<name>. The write is
// idempotent from the plugin's perspective, so it is issued unconditionally.
func (c *Client) EnsureRole(ctx context.Context, r Role) error {
	path := normPath(r.Mount) + "/" + r.RolesPath + "/" + r.Name
	if _, err := c.api.Logical().WriteWithContext(ctx, path, r.Data); err != nil {
		return fmt.Errorf("vault: writing role %s: %w", path, err)
	}
	return nil
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
