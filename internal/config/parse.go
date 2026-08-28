package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Parse decodes and validates the plugin spec from raw ConfigMap data.
func Parse(raw []byte) (*Spec, error) {
	var s Spec
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decoding plugin spec: %w", err)
	}
	s.Settings.ApplyDefaults()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

// Validate checks the spec for internal consistency: known types, exactly one
// binary source per catalog entry, and mounts referencing declared plugins.
func (s *Spec) Validate() error {
	if err := s.Settings.validate(); err != nil {
		return err
	}

	catalog := make(map[string]CatalogEntry, len(s.Catalog))
	for i, c := range s.Catalog {
		switch {
		case c.Name == "":
			return fmt.Errorf("catalog[%d]: name is required", i)
		case !validType(c.Type):
			return fmt.Errorf("catalog[%d] (%s): invalid type %q", i, c.Name, c.Type)
		case c.Version == "":
			return fmt.Errorf("catalog[%d] (%s): version is required", i, c.Name)
		}
		if err := c.Source.validate(); err != nil {
			return fmt.Errorf("catalog[%d] (%s): %w", i, c.Name, err)
		}
		catalog[c.Name] = c
	}

	mountPaths := make(map[string]bool, len(s.Mounts))
	for i, m := range s.Mounts {
		switch {
		case m.Path == "":
			return fmt.Errorf("mounts[%d]: path is required", i)
		case m.Plugin == "":
			return fmt.Errorf("mounts[%d] (%s): plugin is required", i, m.Path)
		case !validType(m.Type):
			return fmt.Errorf("mounts[%d] (%s): invalid type %q", i, m.Path, m.Type)
		case m.Version == "":
			return fmt.Errorf("mounts[%d] (%s): version is required", i, m.Path)
		}
		if _, ok := catalog[m.Plugin]; !ok {
			return fmt.Errorf("mounts[%d] (%s): references unknown plugin %q", i, m.Path, m.Plugin)
		}
		mountPaths[normMountPath(m.Path)] = true
	}

	for i := range s.Roles {
		r := &s.Roles[i]
		switch {
		case r.Mount == "":
			return fmt.Errorf("roles[%d]: mount is required", i)
		case r.Name == "":
			return fmt.Errorf("roles[%d] (%s): name is required", i, r.Mount)
		case !mountPaths[normMountPath(r.Mount)]:
			return fmt.Errorf("roles[%d] (%s): references unknown mount %q", i, r.Name, r.Mount)
		case r.Name == "config":
			// Hard invariant: vpm must never write <mount>/config; that path is a
			// separately-seeded zero-secret and is never reconciled here.
			return fmt.Errorf("roles[%d] (%s): name %q is reserved (vpm never writes <mount>/config)", i, r.Mount, r.Name)
		case strings.Contains(r.Name, "/"):
			return fmt.Errorf("roles[%d] (%s): name %q must not contain %q", i, r.Mount, r.Name, "/")
		}
		// Default and validate the subpath between the mount and the role name.
		// Empty -> `roles` (classic <mount>/roles/<name> layout). The value is
		// trimmed of surrounding slashes so it composes cleanly with the mount.
		normalized, err := normRolesPath(r.RolesPath)
		if err != nil {
			return fmt.Errorf("roles[%d] (%s): %w", i, r.Mount, err)
		}
		r.RolesPath = normalized
	}
	return nil
}

// DefaultRolesPath is the subpath used when a RoleEntry omits rolesPath. It
// reproduces the classic <mount>/roles/<name> layout.
const DefaultRolesPath = "roles"

// normRolesPath defaults, trims, and validates a RoleEntry.RolesPath. An omitted
// (empty) value becomes DefaultRolesPath. Otherwise surrounding slashes are
// trimmed and the result is rejected if it trims to empty (e.g. "/"), or
// contains an empty segment (e.g. "a//b") or a "." or ".." segment — kept
// generic so vpm stays plugin-agnostic.
func normRolesPath(p string) (string, error) {
	if p == "" {
		return DefaultRolesPath, nil
	}
	trimmed := strings.Trim(p, "/")
	if trimmed == "" {
		return "", fmt.Errorf("rolesPath %q must not be empty after trimming slashes", p)
	}
	for _, seg := range strings.Split(trimmed, "/") {
		switch seg {
		case "":
			return "", fmt.Errorf("rolesPath %q must not contain an empty segment", p)
		case ".", "..":
			return "", fmt.Errorf("rolesPath %q must not contain a %q segment", p, seg)
		}
	}
	return trimmed, nil
}

// normMountPath trims surrounding slashes so declared mounts and role.mount
// references compare cleanly, matching internal/vault.normPath.
func normMountPath(p string) string { return strings.Trim(p, "/") }

func (s Source) validate() error {
	switch {
	case s.URL == "" && s.Image == "":
		return fmt.Errorf("source: one of url or image is required")
	case s.URL != "" && s.Image != "":
		return fmt.Errorf("source: only one of url or image may be set")
	case s.Image != "" && s.Path == "":
		return fmt.Errorf("source: path is required when image is set")
	}
	return nil
}

func (s Settings) validate() error {
	switch s.PruneMode {
	case PruneFull, PruneDeregister, PruneNever:
	default:
		return fmt.Errorf("settings: invalid pruneMode %q (want full|deregister|never)", s.PruneMode)
	}
	switch s.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("settings: invalid logLevel %q (want debug|info|warn|error)", s.LogLevel)
	}
	if s.ResyncInterval.Duration() <= 0 {
		return fmt.Errorf("settings: resyncInterval must be positive")
	}
	return nil
}

func validType(t PluginType) bool {
	switch t {
	case PluginTypeSecret, PluginTypeAuth, PluginTypeDatabase:
		return true
	default:
		return false
	}
}
