package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "vault-plugin-manager",
	Short: "Reconciles Vault plugins from a Kubernetes ConfigMap",
	Long: "vault-plugin-manager runs alongside a HashiCorp Vault server, watches a " +
		"ConfigMap describing desired plugins and versions, and reconciles Vault: it " +
		"copies plugin binaries onto every Vault pod, registers them in the catalog, " +
		"and manages the secret/auth engine mounts that use them.",
}

func init() {
	rootCmd.AddCommand(serveCmd)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
