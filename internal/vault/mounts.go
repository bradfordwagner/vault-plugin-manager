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
	tune, drift := mountTune(existing.PluginVersion, existing.Description, existing.Options, m)
	if !drift {
		return false, nil
	}
	if err := c.api.Sys().TuneMountAllowNilWithContext(ctx, path, tune); err != nil {
		return false, fmt.Errorf("vault: tuning secret mount %q: %w", path, err)
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
	tune, drift := mountTune(existing.PluginVersion, existing.Description, existing.Options, m)
	if !drift {
		return false, nil
	}
	// Auth mounts are tuned under the "auth/" prefix.
	if err := c.api.Sys().TuneMountAllowNilWithContext(ctx, "auth/"+path, tune); err != nil {
		return false, fmt.Errorf("vault: tuning auth mount %q: %w", path, err)
	}
	return true, nil
}

// mountTune builds the tune input that brings an existing mount in line with m,
// and reports whether anything actually differs.
//
// Version, description AND options are all reconciled. Description and options
// are only sent on the initial enable otherwise, which made an edit to either a
// silent no-op: the change log reported it (config.Diff emits `description` and
// `options`), the reconcile succeeded, and Vault kept the old value forever --
// breaking the rule that a logged change maps to the work it causes.
//
// Only the options the spec DECLARES are compared, and a tune carries the live
// options with the declared ones merged over them. Two reasons: Vault maintains
// options of its own on some engines (demanding an exact map would tune on every
// pass, the same trap the role comparison avoids), and a tune that sent only the
// declared keys would DROP the rest.
//
// The ownership marker is added only to a mount that already carries it. A mount
// that exists at a declared path but was not created by this manager stays
// unmarked, so it stays outside the prune pass -- adopting it here would make
// somebody else's mount deletable the moment the path left the ConfigMap.
func mountTune(existingVersion, existingDesc string, existingOpts map[string]string, m Mount) (api.TuneMountConfigInput, bool) {
	var tune api.TuneMountConfigInput
	drift := false

	if !sameVersion(existingVersion, m.Version) {
		tune.PluginVersion = &m.Version
		drift = true
	}
	if existingDesc != m.Description {
		tune.Description = &m.Description
		drift = true
	}
	if merged, changed := mergedOptions(existingOpts, m.Options); changed {
		tune.Options = &merged
		drift = true
	}
	return tune, drift
}

// mergedOptions overlays the declared options on the live ones, reporting
// whether that changes anything.
func mergedOptions(existing, declared map[string]string) (map[string]string, bool) {
	merged := make(map[string]string, len(existing)+len(declared))
	for k, v := range existing {
		merged[k] = v
	}
	changed := false
	for k, v := range declared {
		if merged[k] != v {
			merged[k] = v
			changed = true
		}
	}
	return merged, changed
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
