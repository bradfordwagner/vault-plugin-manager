package config

import (
	"fmt"

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
	}
	return nil
}

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
