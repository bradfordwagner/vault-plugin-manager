package serve

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"vault-plugin-manager/internal/args"
	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/fetch"
	"vault-plugin-manager/internal/health"
	"vault-plugin-manager/internal/k8s"
	"vault-plugin-manager/internal/logging"
	"vault-plugin-manager/internal/reconcile"
	"vault-plugin-manager/internal/vault"
)

// serviceAccountNamespaceFile is where the in-cluster namespace is projected.
const serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// startupGrace is how long liveness holds before the reconcile loop has taken
// over the watchdog. It must cover everything Run does first — the bounded
// Vault login retry (3m) plus the informer cache sync — or a pod waiting on a
// Vault role that has not landed yet gets restarted mid-wait.
const startupGrace = 5 * time.Minute

// Run wires up the server: it validates configuration, authenticates to Vault
// and Kubernetes, then runs the reconcile loop until the context is cancelled.
func Run(ctx context.Context, a args.ServeArgs) error {
	if err := validate(a); err != nil {
		return err
	}
	if err := resolveDefaults(&a); err != nil {
		return err
	}

	l := logging.Log().With("cmd", "serve")
	l.With("config", redact(a)).Info("starting vault-plugin-manager")

	// Probe server first: Vault auth below retries for minutes during cluster
	// ignition, and the kubelet must be able to see "up but not ready yet"
	// rather than an unanswered port.
	hs := health.New(health.Config{
		StartupGrace: startupGrace,
		// Replaced from the ConfigMap's settings on the first reconcile pass.
		TokenGracePeriod: config.DefaultTokenGracePeriod,
		TokenFailTimeout: config.DefaultTokenFailTimeout,
		WatchGracePeriod: config.DefaultWatchGracePeriod,
	})
	if a.HealthAddr != "" {
		stop, err := health.Serve(ctx, a.HealthAddr, hs)
		if err != nil {
			return err
		}
		defer stop()
	} else {
		l.Warn("health_addr is empty; liveness/readiness probes are disabled")
	}

	// Vault client: authenticate now (retrying a not-yet-authorized role for a
	// bounded window, then failing) and keep the token maintained in the
	// background until shutdown.
	vc, err := vault.New(vault.Config{
		Addr:       a.VaultAddr,
		CACert:     a.VaultCACert,
		SkipVerify: a.VaultSkipVerify,
		AuthMount:  a.VaultAuthMount,
		Role:       a.VaultAuthRole,
		// Token health feeds the probes: readiness drops after the ConfigMap's
		// tokenGracePeriod, liveness after tokenFailTimeout.
		TokenObserver: hs,
	})
	if err != nil {
		return err
	}
	if err := vc.Authenticate(ctx); err != nil {
		return fmt.Errorf("authenticating to Vault: %w", err)
	}
	l.Info("authenticated to Vault via Kubernetes auth")

	// Kubernetes client for ConfigMap watching, Vault pod discovery, and exec-copy.
	kc, err := k8s.New()
	if err != nil {
		return fmt.Errorf("building Kubernetes client: %w", err)
	}
	l.Info("kubernetes client initialized")

	// Reconciler + runner: watch the ConfigMap and drive Vault toward it.
	rec := reconcile.New(vc, kc, fetch.NewClient(fetch.WithInsecureOCI(a.OCIInsecure)), reconcile.Config{
		VaultNamespace:   a.VaultNamespace,
		VaultPodSelector: a.VaultPodSelector,
		VaultContainer:   a.VaultContainer,
		PluginDir:        a.PluginDir,
	})
	runner := reconcile.NewRunner(rec, kc, a.ConfigMapNamespace, a.ConfigMapName, a.ConfigMapKey, hs)

	l.Info("starting reconcile loop")
	if err := runner.Run(ctx); err != nil {
		return err
	}
	l.Info("shutting down")
	return nil
}

// resolveDefaults fills namespace fields from the in-cluster ServiceAccount when
// they were left blank.
func resolveDefaults(a *args.ServeArgs) error {
	if a.ConfigMapNamespace != "" && a.VaultNamespace != "" {
		return nil
	}
	ns, err := ownNamespace()
	if err != nil {
		return fmt.Errorf("namespace not set and could not be inferred: %w", err)
	}
	if a.ConfigMapNamespace == "" {
		a.ConfigMapNamespace = ns
	}
	if a.VaultNamespace == "" {
		a.VaultNamespace = ns
	}
	return nil
}

func ownNamespace() (string, error) {
	b, err := os.ReadFile(serviceAccountNamespaceFile)
	if err != nil {
		return "", err
	}
	ns := strings.TrimSpace(string(b))
	if ns == "" {
		return "", fmt.Errorf("%s was empty", serviceAccountNamespaceFile)
	}
	return ns, nil
}

func validate(a args.ServeArgs) error {
	var missing []string
	if a.VaultAddr == "" {
		missing = append(missing, "vault_addr")
	}
	if a.VaultAuthRole == "" {
		missing = append(missing, "vault_auth_role")
	}
	if a.ConfigMapName == "" {
		missing = append(missing, "configmap_name")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	// Runtime tunables (pruneMode, resyncInterval, logLevel) are validated where
	// they live — in internal/config when the ConfigMap is parsed.
	return nil
}

// redact returns a loggable copy of the args with nothing secret in it. The
// current args carry no secrets (the SA token is read from disk, not flags), so
// this is a passthrough today but keeps a single place to scrub future fields.
func redact(a args.ServeArgs) args.ServeArgs { return a }
