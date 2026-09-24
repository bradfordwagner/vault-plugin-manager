package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Sections and actions a Change can carry. They are logged verbatim, so keep
// them stable: operators grep them.
const (
	SectionSettings = "settings"
	SectionCatalog  = "catalog"
	SectionMounts   = "mounts"
	SectionRoles    = "roles"

	ActionAdded   = "added"
	ActionRemoved = "removed"
	ActionChanged = "changed"
)

// Change is one difference between two specs: which part of the ConfigMap moved
// and what about it moved. The reconcile Runner logs these so a ConfigMap edit
// is traceable to the work it triggers.
type Change struct {
	Section string // SectionSettings | SectionCatalog | SectionMounts | SectionRoles
	Key     string // identity within the section (see Diff)
	Action  string // ActionAdded | ActionRemoved | ActionChanged
	Detail  string // what differs, e.g. "version 1.0.0 -> 1.1.0"
}

// String renders a Change as one log-friendly line.
func (c Change) String() string {
	s := fmt.Sprintf("%s %s %s", c.Section, c.Key, c.Action)
	if c.Detail != "" {
		s += ": " + c.Detail
	}
	return s
}

// Diff reports how new differs from old, in the order the reconcile loop acts:
// settings, catalog, mounts, roles. Entries are identified by the same keys the
// reconciler uses, so a change maps to the work it causes:
//
//	catalog  name@version   (versions coexist, so a bump is a remove + an add)
//	mounts   type:path
//	roles    mount/rolesPath/name
//
// A nil old means "nothing seen yet": every entry reports as added and settings
// report only where they differ from the defaults. Diff never mutates its
// arguments and returns nil when the specs match.
func Diff(old, new *Spec) []Change {
	if new == nil {
		return nil
	}
	if old == nil {
		base := &Spec{}
		base.Settings.ApplyDefaults()
		old = base
	}

	var out []Change
	out = append(out, diffSettings(old.Settings, new.Settings)...)
	out = append(out, diffCatalog(old.Catalog, new.Catalog)...)
	out = append(out, diffMounts(old.Mounts, new.Mounts)...)
	out = append(out, diffRoles(old.Roles, new.Roles)...)
	return out
}

func diffSettings(old, new Settings) []Change {
	fields := []struct {
		name     string
		old, new string
	}{
		{"pruneMode", string(old.PruneMode), string(new.PruneMode)},
		{"resyncInterval", old.ResyncInterval.Duration().String(), new.ResyncInterval.Duration().String()},
		{"logLevel", old.LogLevel, new.LogLevel},
		{"stallTimeout", old.StallTimeout.Duration().String(), new.StallTimeout.Duration().String()},
		{"tokenGracePeriod", old.TokenGracePeriod.Duration().String(), new.TokenGracePeriod.Duration().String()},
		{"tokenFailTimeout", old.TokenFailTimeout.Duration().String(), new.TokenFailTimeout.Duration().String()},
		{"watchGracePeriod", old.WatchGracePeriod.Duration().String(), new.WatchGracePeriod.Duration().String()},
	}
	var out []Change
	for _, f := range fields {
		if f.old != f.new {
			out = append(out, Change{
				Section: SectionSettings,
				Key:     f.name,
				Action:  ActionChanged,
				Detail:  fmt.Sprintf("%s -> %s", f.old, f.new),
			})
		}
	}
	return out
}

func diffCatalog(old, new []CatalogEntry) []Change {
	oldByKey := make(map[string]CatalogEntry, len(old))
	for _, c := range old {
		oldByKey[catalogKey(c)] = c
	}
	newByKey := make(map[string]CatalogEntry, len(new))
	for _, c := range new {
		newByKey[catalogKey(c)] = c
	}

	var out []Change
	for _, key := range sortedKeys(oldByKey, newByKey) {
		o, hadOld := oldByKey[key]
		n, hasNew := newByKey[key]
		switch {
		case !hadOld:
			out = append(out, Change{SectionCatalog, key, ActionAdded, fmt.Sprintf("type=%s, %s", n.Type, sourceSummary(n.Source))})
		case !hasNew:
			out = append(out, Change{SectionCatalog, key, ActionRemoved, fmt.Sprintf("type=%s", o.Type)})
		default:
			if d := catalogDetail(o, n); d != "" {
				out = append(out, Change{SectionCatalog, key, ActionChanged, d})
			}
		}
	}
	return out
}

func diffMounts(old, new []MountEntry) []Change {
	oldByKey := make(map[string]MountEntry, len(old))
	for _, m := range old {
		oldByKey[mountDiffKey(m)] = m
	}
	newByKey := make(map[string]MountEntry, len(new))
	for _, m := range new {
		newByKey[mountDiffKey(m)] = m
	}

	var out []Change
	for _, key := range sortedKeys(oldByKey, newByKey) {
		o, hadOld := oldByKey[key]
		n, hasNew := newByKey[key]
		switch {
		case !hadOld:
			out = append(out, Change{SectionMounts, key, ActionAdded, fmt.Sprintf("plugin=%s, version=%s", n.Plugin, n.Version)})
		case !hasNew:
			out = append(out, Change{SectionMounts, key, ActionRemoved, fmt.Sprintf("plugin=%s, version=%s (prune candidate)", o.Plugin, o.Version)})
		default:
			if d := mountDetail(o, n); d != "" {
				out = append(out, Change{SectionMounts, key, ActionChanged, d})
			}
		}
	}
	return out
}

func diffRoles(old, new []RoleEntry) []Change {
	oldByKey := make(map[string]RoleEntry, len(old))
	for _, r := range old {
		oldByKey[roleKey(r)] = r
	}
	newByKey := make(map[string]RoleEntry, len(new))
	for _, r := range new {
		newByKey[roleKey(r)] = r
	}

	var out []Change
	for _, key := range sortedKeys(oldByKey, newByKey) {
		o, hadOld := oldByKey[key]
		n, hasNew := newByKey[key]
		switch {
		case !hadOld:
			out = append(out, Change{SectionRoles, key, ActionAdded, fmt.Sprintf("data keys: %s", joinKeys(n.Data))})
		case !hasNew:
			out = append(out, Change{SectionRoles, key, ActionRemoved, "prune candidate"})
		default:
			// Values are the plugin's schema, not ours, so report which keys
			// moved rather than dumping bodies into the log.
			if keys := changedDataKeys(o.Data, n.Data); len(keys) > 0 {
				out = append(out, Change{SectionRoles, key, ActionChanged, "data keys: " + strings.Join(keys, ", ")})
			}
		}
	}
	return out
}

// catalogKey matches reconcile.nvKey: versions coexist in the catalog, so
// name@version is the identity and a version bump reads as a remove + an add.
func catalogKey(c CatalogEntry) string {
	return c.Name + "@" + strings.TrimPrefix(c.Version, "v")
}

// mountDiffKey matches reconcile.mountKey: a path is only unique per mount type.
func mountDiffKey(m MountEntry) string { return string(m.Type) + ":" + normMountPath(m.Path) }

func roleKey(r RoleEntry) string {
	rolesPath := r.RolesPath
	if rolesPath == "" {
		rolesPath = DefaultRolesPath
	}
	return normMountPath(r.Mount) + "/" + rolesPath + "/" + r.Name
}

func catalogDetail(o, n CatalogEntry) string {
	var parts []string
	if o.Type != n.Type {
		parts = append(parts, fmt.Sprintf("type %s -> %s", o.Type, n.Type))
	}
	if o.Source != n.Source {
		parts = append(parts, fmt.Sprintf("source %s -> %s", sourceSummary(o.Source), sourceSummary(n.Source)))
	}
	return strings.Join(parts, ", ")
}

func mountDetail(o, n MountEntry) string {
	var parts []string
	if o.Plugin != n.Plugin {
		parts = append(parts, fmt.Sprintf("plugin %s -> %s", o.Plugin, n.Plugin))
	}
	if strings.TrimPrefix(o.Version, "v") != strings.TrimPrefix(n.Version, "v") {
		parts = append(parts, fmt.Sprintf("version %s -> %s", o.Version, n.Version))
	}
	if o.Config.Description != n.Config.Description {
		parts = append(parts, "description")
	}
	if !reflect.DeepEqual(o.Config.Options, n.Config.Options) {
		parts = append(parts, "options: "+strings.Join(changedStringKeys(o.Config.Options, n.Config.Options), ", "))
	}
	return strings.Join(parts, ", ")
}

// sourceSummary names where a binary comes from without logging a whole source
// block: the checksum and archive-internal binary name are noise here.
func sourceSummary(s Source) string {
	switch {
	case s.Image != "":
		return "image=" + s.Image + " path=" + s.Path
	case s.URL != "":
		return "url=" + s.URL
	default:
		return "source=none"
	}
}

// changedDataKeys lists the keys whose presence or value differs, sorted.
func changedDataKeys(o, n map[string]any) []string {
	var out []string
	for k, ov := range o {
		nv, ok := n[k]
		if !ok || !reflect.DeepEqual(ov, nv) {
			out = append(out, k)
		}
	}
	for k := range n {
		if _, ok := o[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func changedStringKeys(o, n map[string]string) []string {
	oa := make(map[string]any, len(o))
	for k, v := range o {
		oa[k] = v
	}
	na := make(map[string]any, len(n))
	for k, v := range n {
		na[k] = v
	}
	return changedDataKeys(oa, na)
}

func joinKeys[V any](m map[string]V) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "(none)"
	}
	return strings.Join(keys, ", ")
}

// sortedKeys returns the union of both maps' keys, sorted, so diff output is
// deterministic.
func sortedKeys[V any](a, b map[string]V) []string {
	seen := make(map[string]bool, len(a)+len(b))
	keys := make([]string, 0, len(a)+len(b))
	for _, m := range []map[string]V{a, b} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys
}
