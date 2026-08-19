// Command testplugin is a minimal external Vault secrets plugin used only by the
// end-to-end tests. It is a separate Go module so the Vault SDK does not leak
// into the manager's dependency graph.
package main

import (
	"os"

	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/sdk/plugin"
)

func main() {
	meta := &api.PluginAPIClientMeta{}
	flags := meta.FlagSet()
	_ = flags.Parse(os.Args[1:])

	tlsProviderFunc := api.VaultPluginTLSProvider(meta.GetTLSConfig())

	if err := plugin.ServeMultiplex(&plugin.ServeOpts{
		BackendFactoryFunc: Factory,
		TLSProviderFunc:    tlsProviderFunc,
	}); err != nil {
		os.Exit(1)
	}
}
