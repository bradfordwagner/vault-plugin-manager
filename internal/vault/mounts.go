package vault

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/vault/api"
)

// Ownership marker written into a mount's options so pruning only ever touches
// mounts this manager created.
const (
	managedByKey   = "managed-by"
	managedByValue = "vault-plugin-manager"
)

// Mount types.
const (
	MountTypeSecret = "secret"
	MountTypeAuth   = "auth"
)

// Mount describes a desired secret/auth engine mount that consumes a catalog
// plugin at a pinned version.
type Mount struct {
	Path        string // mount path
	Plugin      string // catalog name (becomes the mount's plugin type)
	Type        string // secret | auth
	Version     string // pinned plugin version
	Description string
	Options     map[string]string
}

// ManagedMount is a live mount owned by this manager (carries the marker).
type ManagedMount struct {
	Path    string
	Type    string // secret | auth
	Plugin  string // running plugin type
	Version string // pinned plugin version
}

// EnsureMount enables the mount if missing, or pins it to the desired version if
// it already exists. It returns whether a change was made.
func (c *Client) EnsureMount(ctx context.Context, m Mount) (changed bool, err error) {
	switch m.Type {
	case MountTypeSecret:
		return c.ensureSecretMount(ctx, m)
	case MountTypeAuth:
		return c.ensureAuthMount(ctx, m)
	default:
		return false, fmt.Errorf("vault: unsupported mount type %q", m.Type)
	}
}

func (c *Client) ensureSecretMount(ctx context.Context, m Mount) (bool, error) {
	path := normPath(m.Path)
	mounts, err := c.api.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("vault: listing mounts: %w", err)
	}
	existing := mounts[path+"/"]
	if existing == nil {
		if err := c.api.Sys().MountWithContext(ctx, path, &api.MountInput{
			Type:        m.Plugin,
			Description: m.Description,
			Options:     withManaged(m.Options),
			Config:      api.MountConfigInput{PluginVersion: m.Version},
		}); err != nil {
			return false, fmt.Errorf("vault: enabling secret mount %q: %w", path, err)
		}
		return true, nil
	}
	if sameVersion(existing.PluginVersion, m.Version) {
		return false, nil
	}
	if err := c.api.Sys().TuneMountAllowNilWithContext(ctx, path, api.TuneMountConfigInput{PluginVersion: &m.Version}); err != nil {
		return false, fmt.Errorf("vault: pinning secret mount %q to %s: %w", path, m.Version, err)
	}
	return true, nil
}

func (c *Client) ensureAuthMount(ctx context.Context, m Mount) (bool, error) {
	path := normPath(m.Path)
	auths, err := c.api.Sys().ListAuthWithContext(ctx)
	if err != nil {
		return false, fmt.Errorf("vault: listing auth mounts: %w", err)
	}
	existing := auths[path+"/"]
	if existing == nil {
		if err := c.api.Sys().EnableAuthWithOptionsWithContext(ctx, path, &api.EnableAuthOptions{
			Type:        m.Plugin,
			Description: m.Description,
			Options:     withManaged(m.Options),
			Config:      api.MountConfigInput{PluginVersion: m.Version},
		}); err != nil {
			return false, fmt.Errorf("vault: enabling auth mount %q: %w", path, err)
		}
		return true, nil
	}
	if sameVersion(existing.PluginVersion, m.Version) {
		return false, nil
	}
	// Auth mounts are tuned under the "auth/" prefix.
	if err := c.api.Sys().TuneMountAllowNilWithContext(ctx, "auth/"+path, api.TuneMountConfigInput{PluginVersion: &m.Version}); err != nil {
		return false, fmt.Errorf("vault: pinning auth mount %q to %s: %w", path, m.Version, err)
	}
	return true, nil
}

// DisableMount disables (unmounts) a secret or auth engine. A missing mount is
// treated as success.
func (c *Client) DisableMount(ctx context.Context, path, mountType string) error {
	path = normPath(path)
	switch mountType {
	case MountTypeSecret:
		if err := c.api.Sys().UnmountWithContext(ctx, path); err != nil && !isNotFound(err) {
			return fmt.Errorf("vault: disabling secret mount %q: %w", path, err)
		}
	case MountTypeAuth:
		if err := c.api.Sys().DisableAuthWithContext(ctx, path); err != nil && !isNotFound(err) {
			return fmt.Errorf("vault: disabling auth mount %q: %w", path, err)
		}
	default:
		return fmt.Errorf("vault: unsupported mount type %q", mountType)
	}
	return nil
}

// ListManagedMounts returns the secret and auth mounts owned by this manager
// (those carrying the managed-by marker), for drift detection and pruning.
func (c *Client) ListManagedMounts(ctx context.Context) ([]ManagedMount, error) {
	var out []ManagedMount

	secrets, err := c.api.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault: listing mounts: %w", err)
	}
	for key, mo := range secrets {
		if isManaged(mo.Options) {
			out = append(out, ManagedMount{
				Path:    strings.TrimSuffix(key, "/"),
				Type:    MountTypeSecret,
				Plugin:  mo.Type,
				Version: mo.PluginVersion,
			})
		}
	}

	auths, err := c.api.Sys().ListAuthWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("vault: listing auth mounts: %w", err)
	}
	for key, mo := range auths {
		if isManaged(mo.Options) {
			out = append(out, ManagedMount{
				Path:    strings.TrimSuffix(key, "/"),
				Type:    MountTypeAuth,
				Plugin:  mo.Type,
				Version: mo.PluginVersion,
			})
		}
	}
	return out, nil
}

func withManaged(opts map[string]string) map[string]string {
	out := make(map[string]string, len(opts)+1)
	for k, v := range opts {
		out[k] = v
	}
	out[managedByKey] = managedByValue
	return out
}

func isManaged(opts map[string]string) bool {
	return opts[managedByKey] == managedByValue
}

// normPath trims surrounding slashes so paths compare cleanly against Vault's
// "path/" list keys.
func normPath(p string) string { return strings.Trim(p, "/") }
