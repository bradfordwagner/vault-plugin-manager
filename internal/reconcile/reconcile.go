// Package reconcile drives Vault toward a parsed ConfigMap spec: it fetches and
// exec-copies plugin binaries onto every Vault pod, registers versions in the
// catalog, reconciles secret/auth mounts, reloads changed plugins, and prunes
// what has left the spec.
package reconcile

import (
	"context"
	"fmt"
	"path"
	"strings"

	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/fetch"
	"vault-plugin-manager/internal/logging"
	"vault-plugin-manager/internal/vault"

	"go.uber.org/zap"
)

// pluginFileMode is applied to plugin binaries placed in the Vault plugin_directory.
const pluginFileMode = "0755"

// VaultOps is the subset of the Vault client the reconciler uses.
type VaultOps interface {
	EnsurePlugin(ctx context.Context, p vault.Plugin) (changed bool, err error)
	DeregisterPlugin(ctx context.Context, name, pluginType, version string) error
	EnsureMount(ctx context.Context, m vault.Mount) (changed bool, err error)
	DisableMount(ctx context.Context, path, mountType string) error
	ListManagedMounts(ctx context.Context) ([]vault.ManagedMount, error)
	ReloadPlugin(ctx context.Context, name string) error
}

// PodOps is the subset of the Kubernetes client the reconciler uses.
type PodOps interface {
	ListRunningPods(ctx context.Context, ns, selector string) ([]string, error)
	EnsureFile(ctx context.Context, ns, pod, container, path string, content []byte, sha256hex, mode string) (copied bool, err error)
	RemoveFile(ctx context.Context, ns, pod, container, path string) error
}

// Config holds the static (bootstrap) settings the reconciler needs.
type Config struct {
	VaultNamespace   string
	VaultPodSelector string
	VaultContainer   string
	PluginDir        string
}

// Reconciler is idempotent and level-triggered: each Reconcile drives live state
// toward the given spec regardless of what changed.
type Reconciler struct {
	vault   VaultOps
	pods    PodOps
	fetcher fetch.Fetcher
	cfg     Config
	log     *zap.SugaredLogger
}

// New builds a Reconciler.
func New(v VaultOps, pods PodOps, f fetch.Fetcher, cfg Config) *Reconciler {
	return &Reconciler{
		vault:   v,
		pods:    pods,
		fetcher: f,
		cfg:     cfg,
		log:     logging.Log().With("component", "reconcile"),
	}
}

// Reconcile brings Vault in line with spec.
func (r *Reconciler) Reconcile(ctx context.Context, spec *config.Spec) error {
	pods, err := r.pods.ListRunningPods(ctx, r.cfg.VaultNamespace, r.cfg.VaultPodSelector)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return fmt.Errorf("reconcile: no running Vault pods match selector %q in namespace %s", r.cfg.VaultPodSelector, r.cfg.VaultNamespace)
	}
	r.log.With("pods", len(pods)).Debug("discovered vault pods")

	// Plugins whose catalog registration or a consuming mount changed; reloaded
	// once at the end so running instances pick up the new binary/version.
	reload := make(map[string]bool)

	// 1. Ensure binaries present on every pod, then register the catalog version.
	for _, c := range spec.Catalog {
		fileName := pluginFileName(c.Name, c.Version)
		dest := path.Join(r.cfg.PluginDir, fileName)

		res, err := r.fetcher.Fetch(ctx, toRequest(c))
		if err != nil {
			return fmt.Errorf("reconcile: fetching %s@%s: %w", c.Name, c.Version, err)
		}

		for _, pod := range pods {
			copied, err := r.pods.EnsureFile(ctx, r.cfg.VaultNamespace, pod, r.cfg.VaultContainer, dest, res.Binary, res.SHA256, pluginFileMode)
			if err != nil {
				return fmt.Errorf("reconcile: placing %s on pod %s: %w", fileName, pod, err)
			}
			if copied {
				r.log.With("plugin", c.Name, "version", c.Version, "pod", pod).Info("copied plugin binary")
			}
		}

		changed, err := r.vault.EnsurePlugin(ctx, vault.Plugin{
			Name:    c.Name,
			Type:    string(c.Type),
			Version: c.Version,
			Command: fileName,
			SHA256:  res.SHA256,
		})
		if err != nil {
			return fmt.Errorf("reconcile: registering %s@%s: %w", c.Name, c.Version, err)
		}
		if changed {
			r.log.With("plugin", c.Name, "version", c.Version).Info("registered plugin version")
			reload[c.Name] = true
		}
	}

	// 2. Reconcile mounts and track desired state for pruning.
	desiredMounts := make(map[string]bool)
	desiredVersions := make(map[string]bool)
	for _, c := range spec.Catalog {
		desiredVersions[nvKey(c.Name, c.Version)] = true
	}
	for _, m := range spec.Mounts {
		desiredMounts[mountKey(m.Path, string(m.Type))] = true
		desiredVersions[nvKey(m.Plugin, m.Version)] = true

		changed, err := r.vault.EnsureMount(ctx, vault.Mount{
			Path:        m.Path,
			Plugin:      m.Plugin,
			Type:        string(m.Type),
			Version:     m.Version,
			Description: m.Config.Description,
			Options:     m.Config.Options,
		})
		if err != nil {
			return fmt.Errorf("reconcile: mount %s: %w", m.Path, err)
		}
		if changed {
			r.log.With("mount", m.Path, "version", m.Version).Info("reconciled mount")
			reload[m.Plugin] = true
		}
	}

	// 3. Reload plugins that changed.
	for name := range reload {
		if err := r.vault.ReloadPlugin(ctx, name); err != nil {
			return fmt.Errorf("reconcile: reloading %s: %w", name, err)
		}
		r.log.With("plugin", name).Info("reloaded plugin")
	}

	// 4. Prune what left the spec.
	return r.prune(ctx, spec.Settings.PruneMode, pods, desiredMounts, desiredVersions)
}

// prune removes manager-owned mounts (and, per mode, their versions/binaries)
// that are no longer desired. It only ever touches mounts carrying the
// managed-by marker.
func (r *Reconciler) prune(ctx context.Context, mode config.PruneMode, pods []string, desiredMounts, desiredVersions map[string]bool) error {
	if mode == config.PruneNever {
		return nil
	}
	managed, err := r.vault.ListManagedMounts(ctx)
	if err != nil {
		return fmt.Errorf("reconcile: listing managed mounts: %w", err)
	}
	for _, mm := range managed {
		if desiredMounts[mountKey(mm.Path, mm.Type)] {
			continue
		}

		if err := r.vault.DisableMount(ctx, mm.Path, mm.Type); err != nil {
			return err
		}
		r.log.With("mount", mm.Path, "mode", string(mode)).Info("pruned mount")

		// Deregister the version this mount used, unless another desired entry
		// still references that name@version.
		if mm.Version == "" || desiredVersions[nvKey(mm.Plugin, mm.Version)] {
			continue
		}
		if err := r.vault.DeregisterPlugin(ctx, mm.Plugin, mm.Type, mm.Version); err != nil {
			return err
		}
		r.log.With("plugin", mm.Plugin, "version", mm.Version).Info("deregistered plugin version")

		if mode == config.PruneFull {
			dest := path.Join(r.cfg.PluginDir, pluginFileName(mm.Plugin, mm.Version))
			for _, pod := range pods {
				if err := r.pods.RemoveFile(ctx, r.cfg.VaultNamespace, pod, r.cfg.VaultContainer, dest); err != nil {
					return err
				}
			}
			r.log.With("plugin", mm.Plugin, "version", mm.Version).Info("removed plugin binary")
		}
	}
	return nil
}

// pluginFileName is the on-disk name (in plugin_directory) carrying the version
// metadata Vault needs, e.g. "vault-plugin-secrets-foo-0.3.1".
func pluginFileName(name, version string) string { return name + "-" + version }

// nvKey identifies a plugin version. The version is normalized (leading "v"
// trimmed) so our "1.0.0" and Vault's reported "v1.0.0" compare equal.
func nvKey(name, version string) string {
	return name + "@" + strings.TrimPrefix(version, "v")
}
func mountKey(p, mtype string) string { return mtype + ":" + strings.Trim(p, "/") }

func toRequest(c config.CatalogEntry) fetch.Request {
	return fetch.Request{
		URL:    c.Source.URL,
		Image:  c.Source.Image,
		Path:   c.Source.Path,
		Binary: c.Source.Binary,
		SHA256: c.Source.SHA256,
	}
}
