package vault

import (
	"context"
	"fmt"

	"github.com/hashicorp/vault/api"
)

// ReloadScopeGlobal reloads the plugin on every node of the cluster (not just
// the node servicing this request), which is what we want after (re)registering
// a plugin the standby nodes must also pick up.
const ReloadScopeGlobal = "global"

// ReloadPlugin reloads all mounts backed by the named plugin across the cluster.
func (c *Client) ReloadPlugin(ctx context.Context, name string) error {
	if _, err := c.api.Sys().ReloadPluginWithContext(ctx, &api.ReloadPluginInput{
		Plugin: name,
		Scope:  ReloadScopeGlobal,
	}); err != nil {
		return fmt.Errorf("vault: reloading plugin %q: %w", name, err)
	}
	return nil
}
