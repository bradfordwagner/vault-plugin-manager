package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Spec is the parsed content of the watched ConfigMap data key. It follows the
// "catalog + mounts split" model: Catalog registers plugin versions in Vault's
// plugin catalog; Mounts declare the secret/auth engines that consume them and
// pin an active version per mount. Settings holds runtime tunables that can be
// changed by editing the ConfigMap without redeploying the manager.
type Spec struct {
	Settings Settings       `yaml:"settings"`
	Catalog  []CatalogEntry `yaml:"catalog"`
	Mounts   []MountEntry   `yaml:"mounts"`
	Roles    []RoleEntry    `yaml:"roles"`
}

// Settings are the runtime-tunable knobs sourced from the watched ConfigMap.
// They are re-read on every reconcile, so changes take effect without a restart.
type Settings struct {
	// PruneMode controls what happens when a mount or plugin version that the
	// manager owns is removed from the ConfigMap. See the PruneMode constants.
	PruneMode PruneMode `yaml:"pruneMode"`
	// ResyncInterval is how often the manager runs a full reconcile to correct
	// drift in Vault, in addition to reacting to ConfigMap change events.
	ResyncInterval Duration `yaml:"resyncInterval"`
	// LogLevel is the server log verbosity: debug | info | warn | error.
	LogLevel string `yaml:"logLevel"`
}

// PruneMode controls removal behavior when an owned mount/version leaves the ConfigMap.
type PruneMode string

const (
	// PruneFull makes the ConfigMap fully authoritative. When a mount or version
	// is removed, the manager disables the mount, deregisters the version from
	// the Vault catalog, and deletes the binary from every Vault pod's
	// plugin_directory. Use when the ConfigMap is the single source of truth.
	PruneFull PruneMode = "full"
	// PruneDeregister disables the mount and deregisters the version from the
	// catalog, but leaves the binary on disk. Nothing is deleted from the Vault
	// pods, so re-adding the same version to the ConfigMap skips the re-copy.
	// Use when you want fast rollback without re-fetching binaries.
	PruneDeregister PruneMode = "deregister"
	// PruneNever makes the manager add/update only. Removing an entry from the
	// ConfigMap is ignored entirely: the mount stays enabled, the version stays
	// registered, and the binary stays on disk. Cleanup is manual. Safest.
	PruneNever PruneMode = "never"
)

// Defaults applied when a setting is omitted from the ConfigMap.
const (
	DefaultPruneMode      = PruneFull
	DefaultResyncInterval = 5 * time.Minute
	DefaultLogLevel       = "info"
)

// ApplyDefaults fills unset settings with their defaults.
func (s *Settings) ApplyDefaults() {
	if s.PruneMode == "" {
		s.PruneMode = DefaultPruneMode
	}
	if s.ResyncInterval == 0 {
		s.ResyncInterval = Duration(DefaultResyncInterval)
	}
	if s.LogLevel == "" {
		s.LogLevel = DefaultLogLevel
	}
}

// Duration is a time.Duration that unmarshals from a YAML string like "5m".
type Duration time.Duration

// UnmarshalYAML parses a Go duration string (e.g. "30s", "5m", "1h").
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

// PluginType is the Vault plugin type.
type PluginType string

const (
	PluginTypeSecret   PluginType = "secret"
	PluginTypeAuth     PluginType = "auth"
	PluginTypeDatabase PluginType = "database"
)

// CatalogEntry describes a plugin version to register in the Vault catalog and
// the binary source used to place it on the Vault pods.
type CatalogEntry struct {
	Name    string     `yaml:"name"`    // catalog name
	Type    PluginType `yaml:"type"`    // secret | auth | database
	Version string     `yaml:"version"` // version to register
	Source  Source     `yaml:"source"`  // where the binary comes from
}

// Source describes where to fetch a plugin binary. Exactly one of URL or Image
// must be set.
type Source struct {
	URL    string `yaml:"url"`    // HTTPS download (archive or raw binary)
	Image  string `yaml:"image"`  // OCI image reference
	Path   string `yaml:"path"`   // OCI only: path to the binary inside the image rootfs
	SHA256 string `yaml:"sha256"` // optional expected checksum of the binary
	Binary string `yaml:"binary"` // binary name inside an archive (defaults to Name)
}

// MountEntry declares a secret/auth engine mount that consumes a catalog plugin
// at a pinned version.
type MountEntry struct {
	Path    string      `yaml:"path"`    // mount path
	Plugin  string      `yaml:"plugin"`  // references a CatalogEntry.Name
	Type    PluginType  `yaml:"type"`    // secret | auth
	Version string      `yaml:"version"` // active version pinned to this mount
	Config  MountConfig `yaml:"config"`  // mount tuning + plugin options
}

// MountConfig carries optional mount tuning and plugin options.
type MountConfig struct {
	Description string            `yaml:"description"`
	Options     map[string]string `yaml:"options"`
}

// RoleEntry declares a secret-engine role to UPSERT at
// <Mount>/<RolesPath>/<Name>. Data is written verbatim to the plugin, which owns
// the schema. Role bodies are non-secret and git-owned; the engine's own
// `config` is a separately-seeded zero-secret and is NEVER reconciled here.
type RoleEntry struct {
	Mount string `yaml:"mount"`
	Name  string `yaml:"name"`
	// RolesPath is the path segment(s) BETWEEN the mount and the role name.
	// Optional; defaults to `roles`, which reproduces the classic
	// <mount>/roles/<name> layout. An override like `realm/<realm>/roles` places
	// the role at <mount>/realm/<realm>/roles/<name>. vpm stays plugin-agnostic:
	// the plugin owns the schema, vpm owns placement.
	RolesPath string         `yaml:"rolesPath"`
	Data      map[string]any `yaml:"data"`
}
