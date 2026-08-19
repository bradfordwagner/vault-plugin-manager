package args

// ServeArgs holds the configuration for the vault-plugin-manager server.
//
// Each field is bound to a lowercase CLI flag and an uppercase env var of the
// same name (viper folds the two together). See cmd/vault-plugin-manager/serve.go
// for the flag registration and internal/config for the watched ConfigMap schema.
type ServeArgs struct {
	// Vault connection + Kubernetes auth
	VaultAddr       string `mapstructure:"VAULT_ADDR"`        // e.g. https://vault.vault.svc:8200
	VaultAuthMount  string `mapstructure:"VAULT_AUTH_MOUNT"`  // k8s auth mount path
	VaultAuthRole   string `mapstructure:"VAULT_AUTH_ROLE"`   // Vault role bound to this SA
	VaultCACert     string `mapstructure:"VAULT_CA_CERT"`     // path to CA cert for Vault TLS
	VaultSkipVerify bool   `mapstructure:"VAULT_SKIP_VERIFY"` // skip Vault TLS verification

	// Watched ConfigMap
	ConfigMapName      string `mapstructure:"CONFIGMAP_NAME"`      // ConfigMap to watch
	ConfigMapNamespace string `mapstructure:"CONFIGMAP_NAMESPACE"` // defaults to own namespace
	ConfigMapKey       string `mapstructure:"CONFIGMAP_KEY"`       // data key holding the plugin spec

	// Vault pod discovery + exec-copy
	VaultPodSelector string `mapstructure:"VAULT_POD_SELECTOR"` // label selector for Vault pods
	VaultNamespace   string `mapstructure:"VAULT_NAMESPACE"`    // namespace Vault runs in
	VaultContainer   string `mapstructure:"VAULT_CONTAINER"`    // container name to exec into
	PluginDir        string `mapstructure:"PLUGIN_DIR"`         // Vault plugin_directory path
}

// Runtime tunables (prune mode, resync interval, log level) are NOT here on
// purpose: they are sourced from the watched ConfigMap's `settings` block so
// they can be changed without redeploying. See internal/config.Settings.
