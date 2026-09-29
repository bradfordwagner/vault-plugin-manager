package vault

import (
	"context"
	"fmt"
	"strings"

	"vault-plugin-manager/internal/logging"

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

// MountResult reports what EnsureMount did. Changed drives the metric and the
// log line; Reload is deliberately narrower -- only the plugin VERSION behind a
// mount requires running instances to reload, and a reload tears down and
// re-initializes the backend on every HA node, which a description edit has no
// business causing.
type MountResult struct {
	Changed bool
	Reload  bool
}

// EnsureMount enables the mount if missing, or reconciles version, description
// and options if it already exists.
func (c *Client) EnsureMount(ctx context.Context, m Mount) (MountResult, error) {
	switch m.Type {
	case MountTypeSecret:
		return c.ensureSecretMount(ctx, m)
	case MountTypeAuth:
		return c.ensureAuthMount(ctx, m)
	default:
		return MountResult{}, fmt.Errorf("vault: unsupported mount type %q", m.Type)
	}
}

func (c *Client) ensureSecretMount(ctx context.Context, m Mount) (MountResult, error) {
	path := normPath(m.Path)
	mounts, err := c.api.Sys().ListMountsWithContext(ctx)
	if err != nil {
		return MountResult{}, fmt.Errorf("vault: listing mounts: %w", err)
	}
	existing := mounts[path+"/"]
	if existing == nil {
		if err := c.api.Sys().MountWithContext(ctx, path, &api.MountInput{
			Type:        m.Plugin,
			Description: m.Description,
			Options:     withManaged(m.Options),
			Config:      api.MountConfigInput{PluginVersion: m.Version},
		}); err != nil {
			return MountResult{}, fmt.Errorf("vault: enabling secret mount %q: %w", path, err)
		}
		return MountResult{Changed: true, Reload: true}, nil
	}
	live := liveMount{Version: existing.PluginVersion, Description: existing.Description, Options: existing.Options}
	tune, res := mountTune(live, m)
	if !res.Changed {
		return res, nil
	}
	if err := c.api.Sys().TuneMountAllowNilWithContext(ctx, path, tune); err != nil {
		return MountResult{}, fmt.Errorf("vault: tuning secret mount %q: %w", path, err)
	}
	return res, nil
}

func (c *Client) ensureAuthMount(ctx context.Context, m Mount) (MountResult, error) {
	path := normPath(m.Path)
	auths, err := c.api.Sys().ListAuthWithContext(ctx)
	if err != nil {
		return MountResult{}, fmt.Errorf("vault: listing auth mounts: %w", err)
	}
	existing := auths[path+"/"]
	if existing == nil {
		if err := c.api.Sys().EnableAuthWithOptionsWithContext(ctx, path, &api.EnableAuthOptions{
			Type:        m.Plugin,
			Description: m.Description,
			Options:     withManaged(m.Options),
			Config:      api.MountConfigInput{PluginVersion: m.Version},
		}); err != nil {
			return MountResult{}, fmt.Errorf("vault: enabling auth mount %q: %w", path, err)
		}
		return MountResult{Changed: true, Reload: true}, nil
	}
	live := liveMount{Version: existing.PluginVersion, Description: existing.Description, Options: existing.Options}
	tune, res := mountTune(live, m)
	if !res.Changed {
		return res, nil
	}
	// Auth mounts are tuned under the "auth/" prefix.
	if err := c.api.Sys().TuneMountAllowNilWithContext(ctx, "auth/"+path, tune); err != nil {
		return MountResult{}, fmt.Errorf("vault: tuning auth mount %q: %w", path, err)
	}
	return res, nil
}

// liveMount is the part of a mount Vault reports back that this manager
// reconciles.
type liveMount struct {
	Version     string
	Description string
	Options     map[string]string
}

// mountTune builds the tune input that brings an existing mount in line with m.
//
// Version, description AND options are reconciled. Description and options used
// to be sent only on the initial enable, which made an edit to either a silent
// no-op that config.Diff nonetheless reported -- breaking the rule that a logged
// change maps to the work it causes.
//
// Description and options are reconciled ONLY on a mount this manager owns. A
// mount that already existed at a declared path is somebody else's: pruning
// refuses to delete it, so tuning has no business rewriting its description
// either. Its version is still pinned, which is what the spec is really
// declaring.
//
// On an owned mount the declared options are ABSOLUTE: desired is exactly what
// the spec declares plus the ownership marker, so an option REMOVED from the
// ConfigMap is removed from Vault. Deciding that from the spec alone, rather
// than from a memory of what this process last wrote, is what makes a removal
// converge even when the edit happened while the manager was down -- a restart
// wipes any such memory, and reasserting from it would write the stale option
// back. The cost is that an option some future Vault sets by itself on an owned
// mount would be cleared; the e2e declares mount options and asserts the action
// counters stay flat, so that would surface as tune churn rather than silently.
//
// A removal is expressed as an EMPTY VALUE, and an empty live value reads as
// absent, so this converges however Vault's tune handles the option map: Vault
// MERGES a tuned map into the stored one and deletes the keys whose value is
// empty, so simply omitting a key would leave it in place -- tuning (and, since
// options reload, RELOADING) on every pass forever. Were it to replace the map
// instead, the removed key would come back as an empty value, which compares
// equal to absent. Both roads converge; the e2e removes an option and then
// asserts the counters go flat, which is the ground truth for real Vault.
func mountTune(live liveMount, m Mount) (api.TuneMountConfigInput, MountResult) {
	var tune api.TuneMountConfigInput
	var res MountResult

	if !sameVersion(live.Version, m.Version) {
		tune.PluginVersion = &m.Version
		res.Changed = true
		// The binary behind the mount moved, so running instances must reload.
		res.Reload = true
	}
	if !isManaged(live.Options) {
		// Say so: config.Diff reports a description/options edit as intent, and
		// without this the edit is dropped with no action log to explain it.
		if live.Description != m.Description || len(m.Options) > 0 {
			logging.Log().With("component", "vault", "mount", normPath(m.Path)).
				Warn("mount was not created by this manager; leaving its description and options alone (only the version is pinned)")
		}
		return tune, res
	}

	if live.Description != m.Description {
		tune.Description = &m.Description
		res.Changed = true
		// Deliberately NO reload: a description is metadata, and a reload tears
		// down and re-initializes the backend on every HA node.
	}
	if want, differs := desiredOptions(live.Options, m.Options); differs {
		tune.Options = &want
		res.Changed = true
		// Options DO need one: Vault hands a mount's options to the backend as
		// its config at initialization, so a running instance keeps the old
		// values until it is reloaded -- persisted but not in effect.
		res.Reload = true
	}
	return tune, res
}

// desiredOptions returns the option map to tune an owned mount with -- the
// declared options plus the ownership marker, plus an empty value for every live
// option the spec no longer declares, which is how Vault's tune expresses a
// deletion -- and whether that differs from what the mount holds now.
//
// An absent key and an empty value compare equal, which is what makes a removal
// settle: once Vault has dropped the key it is missing from live, and if some
// Vault stored it as an empty value instead, that reads the same.
func desiredOptions(live, declared map[string]string) (map[string]string, bool) {
	want := withManaged(declared)
	for k := range live {
		if _, still := want[k]; !still {
			want[k] = "" // tell Vault to drop it
		}
	}
	for k, v := range want {
		if live[k] != v {
			return want, true
		}
	}
	return want, false
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
