package vault

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/hashicorp/vault/api"
)

// Plugin describes a plugin version to register in the Vault catalog. Command is
// the file name (relative to plugin_directory) placed on the Vault pods, which
// carries the version metadata (e.g. "vault-plugin-secrets-foo-0.3.1").
type Plugin struct {
	Name    string
	Type    string // secret | auth | database
	Version string
	Command string
	SHA256  string
}

// EnsurePlugin registers the plugin version in the catalog if it is missing or
// differs from what is registered. It returns whether a change was made.
func (c *Client) EnsurePlugin(ctx context.Context, p Plugin) (changed bool, err error) {
	pt, err := parsePluginType(p.Type)
	if err != nil {
		return false, err
	}

	existing, err := c.getPlugin(ctx, p.Name, pt, p.Version)
	if err != nil {
		return false, err
	}
	if existing != nil &&
		existing.SHA256 == p.SHA256 &&
		existing.Command == p.Command &&
		existing.Version == p.Version {
		return false, nil // already registered as desired
	}

	if err := c.api.Sys().RegisterPluginWithContext(ctx, &api.RegisterPluginInput{
		Name:    p.Name,
		Type:    pt,
		Version: p.Version,
		Command: p.Command,
		SHA256:  p.SHA256,
	}); err != nil {
		return false, fmt.Errorf("vault: registering plugin %s@%s: %w", p.Name, p.Version, err)
	}
	return true, nil
}

// DeregisterPlugin removes a plugin version from the catalog. Missing versions
// are treated as success (idempotent).
func (c *Client) DeregisterPlugin(ctx context.Context, name, pluginType, version string) error {
	pt, err := parsePluginType(pluginType)
	if err != nil {
		return err
	}
	if err := c.api.Sys().DeregisterPluginWithContext(ctx, &api.DeregisterPluginInput{
		Name:    name,
		Type:    pt,
		Version: version,
	}); err != nil && !isNotFound(err) {
		return fmt.Errorf("vault: deregistering plugin %s@%s: %w", name, version, err)
	}
	return nil
}

// getPlugin returns the registered plugin, or nil if it is not registered.
func (c *Client) getPlugin(ctx context.Context, name string, pt api.PluginType, version string) (*api.GetPluginResponse, error) {
	resp, err := c.api.Sys().GetPluginWithContext(ctx, &api.GetPluginInput{
		Name:    name,
		Type:    pt,
		Version: version,
	})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("vault: reading plugin %s@%s: %w", name, version, err)
	}
	return resp, nil
}

// parsePluginType maps our config vocabulary (secret|auth|database) to the Vault
// API plugin type.
func parsePluginType(t string) (api.PluginType, error) {
	pt, err := api.ParsePluginType(t)
	if err != nil || pt == api.PluginTypeUnknown {
		return api.PluginTypeUnknown, fmt.Errorf("vault: unsupported plugin type %q", t)
	}
	return pt, nil
}

// isNotFound reports whether err is a Vault 404 response.
func isNotFound(err error) bool {
	var respErr *api.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}
