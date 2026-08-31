// Package vault wraps the HashiCorp Vault API with the operations the plugin
// manager needs: Kubernetes-auth login (with background renew / re-login), the
// plugin catalog, secret/auth engine mounts, and plugin reloads.
package vault

import (
	"context"
	"fmt"
	"time"

	"vault-plugin-manager/internal/logging"

	"github.com/hashicorp/vault/api"
	kubeauth "github.com/hashicorp/vault/api/auth/kubernetes"
	"go.uber.org/zap"
)

// DefaultServiceAccountTokenPath is where the projected SA token lives in-cluster.
const DefaultServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

// Config configures the Vault client and its Kubernetes-auth login.
type Config struct {
	Addr       string // Vault API address
	CACert     string // path to a CA cert for Vault TLS (optional)
	SkipVerify bool   // skip Vault TLS verification
	AuthMount  string // k8s auth mount path (default "kubernetes")
	Role       string // Vault role bound to this ServiceAccount
	TokenPath  string // SA token path (default DefaultServiceAccountTokenPath)
}

// Client is a Vault API client that keeps its token authenticated via the
// Kubernetes auth method: renewing while the lease is renewable, and re-logging
// in when renewal is no longer possible.
type Client struct {
	api *api.Client
	cfg Config
}

// New builds a Vault API client from cfg. It does not log in; call Authenticate.
func New(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, fmt.Errorf("vault: address is required")
	}
	if cfg.Role == "" {
		return nil, fmt.Errorf("vault: auth role is required")
	}
	if cfg.AuthMount == "" {
		cfg.AuthMount = "kubernetes"
	}
	if cfg.TokenPath == "" {
		cfg.TokenPath = DefaultServiceAccountTokenPath
	}

	apiCfg := api.DefaultConfig()
	apiCfg.Address = cfg.Addr
	tlsCfg := &api.TLSConfig{Insecure: cfg.SkipVerify}
	if cfg.CACert != "" {
		tlsCfg.CACert = cfg.CACert
	}
	if err := apiCfg.ConfigureTLS(tlsCfg); err != nil {
		return nil, fmt.Errorf("vault: configuring TLS: %w", err)
	}
	c, err := api.NewClient(apiCfg)
	if err != nil {
		return nil, fmt.Errorf("vault: creating client: %w", err)
	}
	return &Client{api: c, cfg: cfg}, nil
}

// Initial-login retry bounds. During cluster ignition the Vault role that
// authorizes this ServiceAccount may not exist yet: vault-config-operator
// applies it separately, so the first login can transiently fail with a 403.
// Rather than exiting and handing the wait to Kubernetes' CrashLoopBackOff —
// which is stateful and backs off exponentially to ~5m, so recovery lags well
// past when the role lands — Authenticate retries in-process on a bounded
// backoff. A genuine misconfiguration still fails the pod once the overall
// budget is exhausted.
const (
	initialLoginMaxBackoff = 15 * time.Second
	initialLoginBudget     = 3 * time.Minute
)

// Authenticate performs the initial Kubernetes-auth login, retrying transient
// failures in-process for up to initialLoginBudget before giving up, and then
// maintains the token in the background until ctx is cancelled.
func (c *Client) Authenticate(ctx context.Context) error {
	secret, err := c.initialLogin(ctx)
	if err != nil {
		return err
	}
	go c.maintain(ctx, secret)
	return nil
}

// initialLogin retries the first login on an exponential backoff (capped at
// initialLoginMaxBackoff) until it succeeds, ctx is cancelled, or
// initialLoginBudget elapses. On budget exhaustion it returns the last login
// error so the process exits and the failure is surfaced loudly.
func (c *Client) initialLogin(ctx context.Context) (*api.Secret, error) {
	l := logging.Log().With("component", "vault-auth")
	deadline := time.Now().Add(initialLoginBudget)
	backoff := time.Second
	retried := false
	for {
		secret, err := c.login(ctx)
		if err == nil {
			if retried {
				l.Info("initial Vault login succeeded after retrying")
			}
			return secret, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("vault: initial login failed after %s: %w", initialLoginBudget, err)
		}
		retried = true
		wait := backoff
		if remaining := time.Until(deadline); remaining < wait {
			wait = remaining // don't sleep past the budget
		}
		l.With("error", err).With("retry_in", wait.String()).Warn("initial Vault login failed; retrying")
		if !sleepCtx(ctx, wait) {
			return nil, ctx.Err()
		}
		backoff = nextBackoff(backoff, initialLoginMaxBackoff)
	}
}

func (c *Client) login(ctx context.Context) (*api.Secret, error) {
	authMethod, err := kubeauth.NewKubernetesAuth(
		c.cfg.Role,
		kubeauth.WithMountPath(c.cfg.AuthMount),
		kubeauth.WithServiceAccountTokenPath(c.cfg.TokenPath),
	)
	if err != nil {
		return nil, fmt.Errorf("vault: building k8s auth: %w", err)
	}
	secret, err := c.api.Auth().Login(ctx, authMethod)
	if err != nil {
		return nil, fmt.Errorf("vault: kubernetes login: %w", err)
	}
	if secret == nil || secret.Auth == nil {
		return nil, fmt.Errorf("vault: kubernetes login returned no auth info")
	}
	return secret, nil
}

// maintain keeps the token valid until ctx is cancelled: it renews via a
// lifetime watcher while possible and re-logs in when renewal stops or the token
// is non-renewable.
func (c *Client) maintain(ctx context.Context, secret *api.Secret) {
	l := logging.Log().With("component", "vault-auth")
	for {
		if secret == nil {
			var err error
			secret, err = c.loginWithRetry(ctx, l)
			if err != nil {
				return // ctx cancelled
			}
			l.Info("re-authenticated to Vault")
		}

		if !secret.Auth.Renewable {
			ttl := time.Duration(secret.Auth.LeaseDuration) * time.Second
			l.With("ttl", ttl.String()).Info("token not renewable; re-login scheduled before expiry")
			if !sleepCtx(ctx, renewalLead(ttl)) {
				return
			}
			secret = nil
			continue
		}

		watcher, err := c.api.NewLifetimeWatcher(&api.LifetimeWatcherInput{Secret: secret})
		if err != nil {
			l.With("error", err).Warn("failed to start lifetime watcher; re-login")
			secret = nil
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		go watcher.Start()
		cont := c.watch(ctx, watcher, l)
		watcher.Stop()
		if !cont {
			return // ctx cancelled
		}
		secret = nil // watcher stopped renewing -> re-login
	}
}

// watch consumes renew/done events until the watcher stops or ctx is cancelled.
// It returns false only when ctx is cancelled (the caller should stop).
func (c *Client) watch(ctx context.Context, w *api.LifetimeWatcher, l *zap.SugaredLogger) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case err := <-w.DoneCh():
			if err != nil {
				l.With("error", err).Warn("token renewal stopped with error; will re-login")
			} else {
				l.Info("token no longer renewable; will re-login")
			}
			return true
		case renewal := <-w.RenewCh():
			ttl := time.Duration(renewal.Secret.Auth.LeaseDuration) * time.Second
			l.With("ttl", ttl.String()).Debug("token renewed")
		}
	}
}

func (c *Client) loginWithRetry(ctx context.Context, l *zap.SugaredLogger) (*api.Secret, error) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		secret, err := c.login(ctx)
		if err == nil {
			return secret, nil
		}
		l.With("error", err).With("retry_in", backoff.String()).Warn("login failed; retrying")
		if !sleepCtx(ctx, backoff) {
			return nil, ctx.Err()
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// nextBackoff doubles cur, clamped to max (and to max if cur is already at or
// above it). Both bounds are assumed positive.
func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max || next <= 0 {
		return max
	}
	return next
}

// renewalLead returns how long to wait before re-login for a non-renewable
// token: 90% of its TTL, with a one-second floor.
func renewalLead(ttl time.Duration) time.Duration {
	if ttl <= 0 {
		return time.Second
	}
	if lead := ttl * 9 / 10; lead > time.Second {
		return lead
	}
	return time.Second
}

// sleepCtx sleeps for d or until ctx is cancelled. It returns false if cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
