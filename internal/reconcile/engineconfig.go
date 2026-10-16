package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"vault-plugin-manager/internal/config"
	"vault-plugin-manager/internal/k8s"
	"vault-plugin-manager/internal/metrics"
)

// engineConfigStateKey is the state Secret's only data key. Secret data keys
// cannot contain "/", so every "<mount>/<path>" record shares this one.
const engineConfigStateKey = "engine-configs.json"

// redactedMarker replaces each run of Secret-derived bytes in a scrubbed error.
const redactedMarker = "<redacted>"

// Write reasons logged with "wrote engine config".
const (
	reasonUnrecorded     = "unrecorded"
	reasonMountRecreated = "mount_recreated"
	reasonChanged        = "changed"
)

// engineConfigRecord is what the state Secret holds for one engine config: the
// digest of the body last written and the UUID of the mount it was written to.
type engineConfigRecord struct {
	SHA256    string `json:"sha256"`
	MountUUID string `json:"mountUUID"`
}

// resolvedEngineConfig is an entry with its full body built and digested.
type resolvedEngineConfig struct {
	mount  string
	path   string
	key    string // "<mount>/<path>", the Vault path and the state record key
	body   map[string]any
	digest string
}

// reconcileEngineConfigs writes every engine config whose body or mount moved
// since the state Secret recorded it, then saves the state (SPEC §3a). Every
// error it returns is scrubbed of the Secret values resolved this pass.
func (r *Reconciler) reconcileEngineConfigs(ctx context.Context, spec *config.Spec) error {
	var values []string
	err := r.writeEngineConfigs(ctx, spec, &values)
	return scrubSecrets(err, values)
}

// writeEngineConfigs runs the step; values collects each Secret value it puts
// into a body, for the caller to scrub from the error.
func (r *Reconciler) writeEngineConfigs(ctx context.Context, spec *config.Spec, values *[]string) error {
	records, rv, err := r.loadEngineConfigState(ctx)
	if err != nil {
		return err
	}
	entries, err := r.resolveEngineConfigs(ctx, spec.EngineConfigs, values)
	if err != nil {
		return err
	}
	uuids, err := r.engineConfigMountUUIDs(ctx, entries)
	if err != nil {
		return err
	}

	dirty := false
	var writeErr error
	for _, e := range entries {
		uuid := uuids[e.mount]
		var reason string
		switch rec := records[e.key]; {
		case rec == nil:
			reason = reasonUnrecorded
		case rec.MountUUID != uuid:
			reason = reasonMountRecreated
		case rec.SHA256 != e.digest:
			reason = reasonChanged
		default:
			continue
		}
		err := r.vault.WriteEngineConfig(ctx, e.key, e.body)
		wrote := err == nil
		metrics.VaultActionIf(metrics.ActionEngineConfig, wrote, err)
		if err != nil {
			// Stop here, but still checkpoint the writes that landed so they are
			// not repeated on every pass while this entry keeps failing.
			writeErr = fmt.Errorf("reconcile: engine config %s: %w", e.key, err)
			break
		}
		r.log.With("mount", e.mount, "path", e.path, "reason", reason).Info("wrote engine config")
		records[e.key] = &engineConfigRecord{SHA256: e.digest, MountUUID: uuid}
		dirty = true
	}

	// Drop records of entries that left the spec. Vault is left as it is.
	declared := make(map[string]bool, len(entries))
	for _, e := range entries {
		declared[e.key] = true
	}
	for key := range records {
		if !declared[key] {
			delete(records, key)
			dirty = true
		}
	}

	var saveErr error
	if dirty {
		saveErr = r.saveEngineConfigState(ctx, rv, records)
	}
	return errors.Join(writeErr, saveErr)
}

// stateSecretRef names the state Secret in errors.
func (r *Reconciler) stateSecretRef() string {
	return r.cfg.SecretNamespace + "/" + r.cfg.StateSecretName
}

// loadEngineConfigState reads the recorded writes and the state Secret's
// resourceVersion. Only a missing Secret is empty state (rv ""); every other
// unusable outcome fails closed, since treating it as empty would rewrite
// every entry and reseed credentials a rotating engine has replaced.
func (r *Reconciler) loadEngineConfigState(ctx context.Context) (map[string]*engineConfigRecord, string, error) {
	data, rv, err := r.secrets.GetSecretData(ctx, r.cfg.SecretNamespace, r.cfg.StateSecretName)
	if errors.Is(err, k8s.ErrNotFound) {
		return map[string]*engineConfigRecord{}, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("reconcile: reading engine config state Secret %s: %w", r.stateSecretRef(), err)
	}
	raw, ok := data[engineConfigStateKey]
	if !ok {
		return nil, "", fmt.Errorf("reconcile: engine config state Secret %s has no %s key; fix or delete it", r.stateSecretRef(), engineConfigStateKey)
	}
	var records map[string]*engineConfigRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, "", fmt.Errorf("reconcile: decoding engine config state Secret %s: %w; fix or delete it", r.stateSecretRef(), err)
	}
	if records == nil {
		return nil, "", fmt.Errorf("reconcile: engine config state Secret %s holds null; fix or delete it", r.stateSecretRef())
	}
	for _, key := range slices.Sorted(maps.Keys(records)) {
		if rec := records[key]; rec == nil || rec.SHA256 == "" || rec.MountUUID == "" {
			return nil, "", fmt.Errorf("reconcile: engine config state Secret %s: record %q lacks sha256 or mountUUID; fix or delete it", r.stateSecretRef(), key)
		}
	}
	return records, rv, nil
}

// resolveEngineConfigs builds and digests every body before anything is
// written. Each referenced Secret is read once, so entries sharing a Secret
// resolve from the same snapshot.
func (r *Reconciler) resolveEngineConfigs(ctx context.Context, entries []config.EngineConfigEntry, values *[]string) ([]resolvedEngineConfig, error) {
	ns := r.cfg.SecretNamespace
	snapshot := make(map[string]map[string][]byte)
	out := make([]resolvedEngineConfig, 0, len(entries))
	for _, e := range entries {
		key := e.Mount + "/" + e.Path
		body := make(map[string]any, len(e.Data))
		maps.Copy(body, e.Data)

		if ref := e.SecretRef; ref != nil {
			data, ok := snapshot[ref.Name]
			if !ok {
				d, _, err := r.secrets.GetSecretData(ctx, ns, ref.Name)
				if errors.Is(err, k8s.ErrNotFound) {
					return nil, fmt.Errorf("reconcile: engine config %s: Secret %s/%s not found", key, ns, ref.Name)
				}
				if err != nil {
					return nil, fmt.Errorf("reconcile: engine config %s: reading Secret %s/%s: %w", key, ns, ref.Name, err)
				}
				snapshot[ref.Name] = d
				data = d
			}
			for _, field := range slices.Sorted(maps.Keys(ref.Keys)) {
				secretKey := ref.Keys[field]
				v, ok := data[secretKey]
				switch {
				case !ok:
					return nil, fmt.Errorf("reconcile: engine config %s: Secret %s/%s has no key %q", key, ns, ref.Name, secretKey)
				case len(v) == 0:
					return nil, fmt.Errorf("reconcile: engine config %s: Secret %s/%s key %q is empty", key, ns, ref.Name, secretKey)
				case !utf8.Valid(v):
					return nil, fmt.Errorf("reconcile: engine config %s: Secret %s/%s key %q is not valid UTF-8", key, ns, ref.Name, secretKey)
				}
				s := string(v)
				body[field] = s
				*values = append(*values, s)
			}
		}

		digest, err := engineConfigDigest(key, body)
		if err != nil {
			return nil, fmt.Errorf("reconcile: engine config %s: encoding body: %w", key, err)
		}
		out = append(out, resolvedEngineConfig{mount: e.Mount, path: e.Path, key: key, body: body, digest: digest})
	}
	return out, nil
}

// engineConfigMountUUIDs reads the live mount UUIDs and checks every entry's
// mount has one before any write, so an empty UUID is never recorded.
func (r *Reconciler) engineConfigMountUUIDs(ctx context.Context, entries []resolvedEngineConfig) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	uuids, err := r.vault.SecretMountUUIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("reconcile: engine configs: %w", err)
	}
	for _, e := range entries {
		switch uuid, ok := uuids[e.mount]; {
		case !ok:
			return nil, fmt.Errorf("reconcile: engine config %s: mount %q is not a live secret mount", e.key, e.mount)
		case uuid == "":
			return nil, fmt.Errorf("reconcile: engine config %s: mount %q reports an empty UUID", e.key, e.mount)
		}
	}
	return uuids, nil
}

// saveEngineConfigState writes records back: an update guarded by rv, or a
// create when rv is "".
func (r *Reconciler) saveEngineConfigState(ctx context.Context, rv string, records map[string]*engineConfigRecord) error {
	raw, err := json.Marshal(records)
	if err != nil {
		return fmt.Errorf("reconcile: encoding engine config state: %w", err)
	}
	data := map[string][]byte{engineConfigStateKey: raw}
	if _, err := r.secrets.PutSecretData(ctx, r.cfg.SecretNamespace, r.cfg.StateSecretName, rv, data); err != nil {
		return fmt.Errorf("reconcile: saving engine config state Secret %s: %w", r.stateSecretRef(), err)
	}
	return nil
}

// engineConfigDigest is the hex sha256 of the canonical JSON of {path, body}.
// encoding/json sorts map keys at every depth, so the digest does not depend on
// the order the ConfigMap declared them in.
func engineConfigDigest(path string, body map[string]any) (string, error) {
	raw, err := json.Marshal(struct {
		Path string         `json:"path"`
		Body map[string]any `json:"body"`
	}{path, body})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// scrubSecrets returns err's text with every occurrence of each value, raw or
// JSON-escaped, replaced. Every byte covered by any occurrence is masked first
// and each maximal masked run becomes one marker, so overlapping or nested
// values leave no fragment behind. The result is a new error that does not
// unwrap to the original.
func scrubSecrets(err error, values []string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	masked := make([]bool, len(msg))
	for _, v := range values {
		for _, p := range secretPatterns(v) {
			for from := 0; from < len(msg); {
				i := strings.Index(msg[from:], p)
				if i < 0 {
					break
				}
				start := from + i
				for j := start; j < start+len(p); j++ {
					masked[j] = true
				}
				from = start + 1
			}
		}
	}
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		switch {
		case !masked[i]:
			b.WriteByte(msg[i])
		case i == 0 || !masked[i-1]:
			b.WriteString(redactedMarker)
		}
	}
	return errors.New(b.String())
}

// secretPatterns is v and, when it differs, v as it appears inside a JSON string.
func secretPatterns(v string) []string {
	if v == "" {
		return nil
	}
	out := []string{v}
	if raw, err := json.Marshal(v); err == nil {
		if esc := string(raw[1 : len(raw)-1]); esc != v {
			out = append(out, esc)
		}
	}
	return out
}
