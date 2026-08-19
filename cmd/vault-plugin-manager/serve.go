package main

import (
	"vault-plugin-manager/internal/args"
	"vault-plugin-manager/internal/cmds/serve"

	"github.com/bradfordwagner/go-util/flag_helper"
	"github.com/spf13/cobra"
)

var serveArgs args.ServeArgs

func init() {
	fs := serveCmd.Flags()

	// Vault connection + Kubernetes auth
	flag_helper.CreateFlag(fs, &serveArgs.VaultAddr, "vault_addr", "", "", "Vault API address (env VAULT_ADDR)")
	flag_helper.CreateFlag(fs, &serveArgs.VaultAuthMount, "vault_auth_mount", "", "kubernetes", "Vault Kubernetes auth mount path (env VAULT_AUTH_MOUNT)")
	flag_helper.CreateFlag(fs, &serveArgs.VaultAuthRole, "vault_auth_role", "", "", "Vault role bound to this ServiceAccount (env VAULT_AUTH_ROLE)")
	flag_helper.CreateFlag(fs, &serveArgs.VaultCACert, "vault_ca_cert", "", "", "Path to CA cert for Vault TLS (env VAULT_CA_CERT)")
	flag_helper.CreateFlag(fs, &serveArgs.VaultSkipVerify, "vault_skip_verify", "", false, "Skip Vault TLS verification (env VAULT_SKIP_VERIFY)")

	// Watched ConfigMap
	flag_helper.CreateFlag(fs, &serveArgs.ConfigMapName, "configmap_name", "", "", "ConfigMap to watch (env CONFIGMAP_NAME)")
	flag_helper.CreateFlag(fs, &serveArgs.ConfigMapNamespace, "configmap_namespace", "", "", "Namespace of the watched ConfigMap; defaults to own namespace (env CONFIGMAP_NAMESPACE)")
	flag_helper.CreateFlag(fs, &serveArgs.ConfigMapKey, "configmap_key", "", "plugins.yaml", "ConfigMap data key holding the plugin spec (env CONFIGMAP_KEY)")

	// Vault pod discovery + exec-copy
	flag_helper.CreateFlag(fs, &serveArgs.VaultPodSelector, "vault_pod_selector", "", "app.kubernetes.io/name=vault", "Label selector for Vault pods (env VAULT_POD_SELECTOR)")
	flag_helper.CreateFlag(fs, &serveArgs.VaultNamespace, "vault_namespace", "", "", "Namespace Vault runs in; defaults to own namespace (env VAULT_NAMESPACE)")
	flag_helper.CreateFlag(fs, &serveArgs.VaultContainer, "vault_container", "", "vault", "Vault container name to exec into (env VAULT_CONTAINER)")
	flag_helper.CreateFlag(fs, &serveArgs.PluginDir, "plugin_dir", "", "/vault/plugins", "Vault plugin_directory path (env PLUGIN_DIR)")
	flag_helper.CreateFlag(fs, &serveArgs.OCIInsecure, "oci_insecure", "", false, "Allow OCI pulls from plain-HTTP/untrusted-TLS registries (env OCI_INSECURE)")

	// NOTE: runtime tunables (pruneMode, resyncInterval, logLevel) are sourced
	// from the watched ConfigMap's `settings` block, not from flags/env.
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Watch the ConfigMap and continuously reconcile Vault plugins",
	RunE: func(cmd *cobra.Command, _ []string) error {
		flag_helper.Load(&serveArgs)
		return serve.Run(cmd.Context(), serveArgs)
	},
}
