# vault-plugin-manager — Spec

A long-running Go server that runs in the **same namespace as a HashiCorp Vault
server**, watches a **ConfigMap** describing which Vault plugins should exist and
at what versions, and reconciles Vault to match: it copies plugin binaries onto
each Vault pod's filesystem, registers them in the plugin catalog, and manages
the secret/auth engine mounts that use them.

Built on the existing cobra CLI in this repo. The module/binary will be renamed
from the `template_cli` / `testcli` scaffold to `vault-plugin-manager`.

---

## 1. Decisions (locked)

| Area | Decision |
|------|----------|
| Binary delivery | **exec/copy into every Vault pod** matching a label selector (HA-aware). Register once via the Vault API through the service. |
| Binary source | **Both HTTPS-URL and OCI image**, behind a `PluginFetcher` interface. |
| Scope | **Full lifecycle** — register/deregister catalog versions, enable/tune/disable mounts, reload plugins. |
| Reconcile model | **Kubernetes informer** on the ConfigMap + periodic **resync (default 5m)** to correct drift in Vault. |
| Pruning | **Prune fully by default** (disable mount → deregister version → remove binary), **configurable** (`full` \| `deregister` \| `never`). |
| Config schema | **Catalog + mounts split** (a plugin can back multiple mounts). |
| Auth | Vault **Kubernetes auth method** using the manager's ServiceAccount token. |
| Replicas | **1** (single writer). `strategy: Recreate`. Leader election is a later option if HA of the manager is ever needed. |
| Engine config | `engineConfigs` writes `<mount>/<path>` (default `config`) from literal data plus values read from a Kubernetes Secret, **only when that content changes**. The digest of what was last written persists in a vpm-owned state Secret, so a restart does not rewrite. Removing an entry never deletes the config. |

---

## 2. Reconcile loop

Triggered by a ConfigMap change event **or** the periodic resync. Steps:

1. **Load & parse** the ConfigMap (`catalog` + `mounts`).
2. **Authenticate** to Vault via `auth/<mount>/login` with the SA token + role.
   Cache the token; renew before expiry.
3. **Ensure binaries present** — for each `catalog` entry:
   - Fetch via the entry's source (`url` or `image`), extract the binary.
   - Compute sha256; if an expected `sha256` is provided, verify and fail closed on mismatch.
   - Discover Vault pods (label selector). For each pod, check the on-disk file
     via `exec sha256sum <plugin_dir>/<name>-<version>` (confirmed present in the
     official image at `/usr/bin/sha256sum`); if absent or mismatched, stream the
     bytes in via `exec sh -c 'cat > <file>'`, `chmod 0755`, then **re-run
     `sha256sum` on the placed file and verify** it matches before registering.
     Versioned filenames (`<name>-<version>`) carry the version metadata Vault
     needs and let multiple versions coexist on disk.
4. **Register catalog** — `POST sys/plugins/catalog/<type>/<name>` with
   `{ sha256, version, command: <name>-<version> }`. Idempotent: skip if the live
   registration already matches.
5. **Reconcile mounts** — for each desired `mounts` entry:
   - Enable if missing (`sys/mounts/<path>` for secret, `sys/auth/<path>` for auth).
   - Pin `plugin_version` to the entry's version (tune) and set config/options.
   - Reload via `sys/plugins/reload/backend`.
   - Tag manager-owned mounts with a marker (e.g. description/metadata
     `managed-by=vault-plugin-manager`) so pruning never touches foreign mounts.
6. **Write engine configs**: for each `engineConfigs` entry, build the body from
   `data` plus the referenced Secret's values and write it to `<mount>/<path>` if
   its digest or its mount's UUID differs from the record in the state Secret
   (see §3a). Runs
   after mounts and reloads (the mount must exist) and before role reconciliation
   (a plugin may need its config before it accepts roles).
7. **Prune** (per mode) — mounts/versions that are manager-owned but no longer in
   the ConfigMap:
   - `full`: disable mount → deregister version → `rm` binary on each pod.
   - `deregister`: disable mount → deregister version, **leave** binary on disk.
   - `never`: no-op (add/update only).

Reconcile is **idempotent** and **level-triggered** — every run drives live state
toward the ConfigMap regardless of what changed.

---

**Change logging.** Each pass logs the ConfigMap diff (`config.Diff`) before
acting: one line per change, keyed as the reconciler keys entries
(`name@version`, `type:path`, `mount/rolesPath/name`), with section / key /
action as structured fields. The reconciler's own logs then record the work
performed. A change is logged once, not on every resync; role changes report
which data keys moved, never the values.

---

## 3. ConfigMap schema

One data key (e.g. `plugins.yaml`):

```yaml
settings:                            # runtime tunables (optional; defaults shown)
  pruneMode: full                    # full | deregister | never
  resyncInterval: 5m
  logLevel: info

catalog:
  - name: vault-plugin-secrets-foo   # catalog name
    type: secret                     # secret | auth | database
    version: "0.3.1"                 # version to register
    source:
      url: https://releases.example.com/foo_0.3.1_linux_amd64.zip   # one of url|image
      image: ghcr.io/org/vault-plugin-secrets-foo:0.3.1
      path: /plugin/foo              # OCI only: path to the binary inside the image rootfs (configurable)
      sha256: "<optional expected checksum of the binary>"
      binary: foo                    # binary name inside the archive (optional; defaults to name)

mounts:
  - path: foo                        # mount path
    plugin: vault-plugin-secrets-foo # references a catalog[].name
    type: secret                     # secret | auth
    version: "0.3.1"                 # active version pinned to this mount
    config:                          # mount tuning + plugin options (optional)
      description: "Foo secrets engine"
      options: {}

engineConfigs:                       # plugin engine config writes (optional)
  - mount: foo                       # references a mounts[].path
    path: config                     # optional; default `config`; written to <mount>/<path>
    data:                            # non-secret literal fields, written as-is
      app_id: 4627856
    secretRef:                       # optional; a Secret in the ConfigMap's namespace
      name: foo-engine-credentials
      keys:                          # <vault field>: <secret data key>
        prv_key: private-key.pem
```

Notes:
- `catalog` may register multiple versions of the same plugin; each `mounts` entry
  pins the **active** version for that mount.
- `type: database` plugins register in the catalog but are consumed via the
  `database` secrets engine rather than a standalone mount (mount handling for
  database plugins TBD in implementation).

---

## 3a. Engine configs

`engineConfigs` writes a plugin's own configuration endpoint (typically
`<mount>/config`) from the ConfigMap plus a Kubernetes Secret, so credentials
such as a GitHub App private key reach the engine without anything outside the
cluster writing Vault engine config. The plugin owns the schema; vpm owns the
path and the timing of the write.

**Body.** The written body is `data` plus one field per `secretRef.keys` entry,
set to that Secret data key's value as a string. vpm always writes the whole
body, never a partial update.

**Validation** (the spec is rejected as a whole, as for any other section):

- `mount` is required and must reference a declared `mounts[]` entry of
  `type: secret`. The request path is `<mount>/<path>`; auth methods keep their
  config under `auth/<mount>/...`, which this section does not address, so a
  reference to an auth mount is rejected.
- `path` defaults to `config`; surrounding slashes are trimmed, and empty, `.` or
  `..` segments are rejected (the same rules as `roles[].rolesPath`).
- `(mount, path)` is unique across entries.
- At least one of `data` or `secretRef` is set. A `secretRef` needs a `name` and a
  non-empty `keys`.
- A Vault field may not appear in both `data` and `secretRef.keys`.
- `path` may not equal, or sit under, any `rolesPath` declared on the same mount.
  Role pruning under `pruneMode: full` lists every declared `rolesPath` and
  deletes what the spec does not name, so an engine config there would be deleted
  as an undeclared role. This also keeps the two sections from writing one path.

`roles` still rejects a role named `config`: engine config is written only
through this section, and only on a content change.

**Write on change, not on every pass.** Engine config cannot be reconciled by
read-compare-write the way roles are: secret fields read back redacted (the
GitHub engine returns `prv_key: <configured>`), and some engines rotate a seeded
credential and purge the original (the Keycloak engine's `client_secret`), so a
rewrite with the seeded value breaks them. Each pass therefore:

1. Reads the state Secret (§3a "State"). NotFound means no writes are recorded.
2. Resolves every entry before writing any: GETs each `secretRef` Secret and
   builds the body. A missing Secret or key fails the pass here, with nothing
   written, like any other reconcile error: catalog and mounts have already
   converged, roles and pruning wait.
3. Reads the live UUID of each referenced mount from `sys/mounts`. Vault assigns a
   new UUID whenever a mount is enabled, so a mount that was disabled and
   re-enabled (by vpm or anyone else) is recognizable even though its path is not.
4. For each entry, computes `sha256` over the canonical JSON of
   `{vault path, body}`. When both the digest and the mount UUID equal the
   recorded ones the entry is skipped; otherwise vpm writes the body to
   `<mount>/<path>` and records the new digest and UUID. Including the path means
   an entry moved to a new path is written; comparing the UUID means a recreated
   mount, whose config went with it, is written.
5. Drops records for entries no longer in the spec, so an entry that is removed
   and later re-added is written again.
6. Saves the state Secret whenever the step leaves it changed, **including when a
   later entry's write fails**: every write that landed is checkpointed before the
   error is returned, so one failing entry cannot make the others rewrite on every
   pass. The save is an update guarded by `resourceVersion`, or a create when the
   Secret does not exist yet. A failed save is returned alongside any write error.

Referenced Secrets are read on every pass, not watched: a rotated Secret value is
applied within `resyncInterval`. A ConfigMap edit still reconciles immediately.

**Ordering.** vpm writes Vault first and records the digest second. A crash, or a
failed state save, between the two rewrites the same content on the next pass.
That is harmless for an engine whose config is static (GitHub), and only matters
for a rotating engine if the rotation happened inside that window.

**Adoption.** An entry with no record is written on the first pass that sees it.
Moving an already-configured engine under `engineConfigs` therefore overwrites
its config once with the ConfigMap's values.

**Pruning.** Removing an entry never deletes or rewrites the config in Vault,
under any `pruneMode`; only its record is dropped. Engine config is not safely
deletable in general (deleting it can strand roles and leases that depend on it).

**State.** The state Secret (`state_secret_name`, chart default
`<fullname>-state`) lives in the ConfigMap's namespace and is owned by vpm. It has
one data key, `engine-configs.json`, holding a map of `<mount>/<path>` to
`{"sha256": "<hex digest of the body last written>", "mountUUID": "<uuid of the
mount it was written to>"}` (Secret data keys cannot contain `/`, so the records
share one key). It is a Secret, not a ConfigMap, because the digests are derived
from secret values and a short secret value can be recovered from its hash by
guessing. Deleting a record from it, or deleting the whole Secret, forces a
rewrite on the next pass.

**Logging and metrics.** A write logs `wrote engine config` at Info with `mount`,
`path` and `reason` (`unrecorded` when there was no record, `mount_recreated`
when the mount UUID moved, `changed` otherwise), never values. It counts as the
`engine_config` action through `metrics.VaultActionIf`, so steady state is flat.
`vpm_spec_entries` carries `kind=engineConfigs`. The ConfigMap change log keys
entries as `mount/path` and reports which `data` keys and which `secretRef`
fields moved, never values; a Secret content change is not a ConfigMap change and
shows up only as the write.

---

## 4. Manager configuration

Split by lifecycle:

**Bootstrap config** (flags / env via viper + flag_helper) — needed before the
ConfigMap can be read:

| Flag / env | Default | Purpose |
|------------|---------|---------|
| `VAULT_ADDR` | — | Vault API address (service DNS, e.g. `https://vault.vault.svc:8200`) |
| `vault_auth_mount` | `kubernetes` | Vault k8s auth mount path |
| `vault_auth_role` | — | Vault role bound to the manager SA |
| `vault_ca_cert` / `vault_skip_verify` | — | TLS to Vault |
| `configmap_name` | — | ConfigMap to watch |
| `configmap_namespace` | own namespace (SA file) | where the ConfigMap lives |
| `configmap_key` | `plugins.yaml` | data key holding the spec |
| `vault_pod_selector` | `app.kubernetes.io/name=vault` | label selector for Vault pods |
| `vault_namespace` | own namespace | where Vault pods run |
| `vault_container` | `vault` | container name to exec into |
| `plugin_dir` | `/vault/plugins` | Vault `plugin_directory` path |
| `health_addr` | `:8080` | liveness/readiness listen address; empty disables the probe server |
| `state_secret_name` | `vault-plugin-manager-state` | vpm-owned Secret (in `configmap_namespace`) recording the digest of each engine config written (§3a) |

**Runtime tunables** — live in the watched ConfigMap's `settings:` block, re-read
each reconcile so they change without a redeploy:

| Setting | Default | Purpose |
|---------|---------|---------|
| `pruneMode` | `full` | `full` \| `deregister` \| `never` (see §2 step 7) |
| `resyncInterval` | `5m` | periodic drift reconcile |
| `logLevel` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `stallTimeout` | `10m` | how long one reconcile pass may run before liveness calls the loop wedged |
| `tokenGracePeriod` | `2m` | how long the Vault token may be invalid before readiness drops |
| `tokenFailTimeout` | `15m` | how long the Vault token may be invalid before liveness drops (must be >= grace) |
| `watchGracePeriod` | `2m` | how long the ConfigMap watch may fail before readiness drops |

**Health probes** (`internal/health`, served on `health_addr`):

- `/healthz` (liveness) is a watchdog on the reconcile loop, not an HTTP
  echo — the manager serves no traffic, so "responding" has to mean "the loop
  is turning". The Runner declares its next deadline before each wait
  (`resyncInterval + stallTimeout`) and before each pass (`stallTimeout`);
  missing it returns 503 and the kubelet restarts the pod.
- `/readyz` (readiness) is a startup gate: true on the first clean pass, then
  sticky for reconcile errors, so rollouts gate on a real reconcile without
  flapping on a transient Vault error. An absent/unparseable ConfigMap counts as
  clean — that's the user's spec, not a broken manager.
- **Vault token health feeds both probes.** The client's login/renew loop
  reports through a `vault.TokenObserver`; `health.State` implements it. Readiness
  drops once the token has been invalid for `tokenGracePeriod`, liveness only
  after `tokenFailTimeout` — restarting the pod does not fix a Vault that is
  down, it only re-runs the login the client already retries on a backoff.
  Repeated failures do not restart the grace clock.
- **ConfigMap watcher health feeds both probes.** `WatchConfigMap`
  (`internal/k8s/informer.go`) returns the informer; a stopped informer
  (`IsStopped`) fails liveness immediately, since the loop would otherwise
  reconcile its stale cache forever while looking healthy. A watch that is
  erroring but still relisting fails readiness after `watchGracePeriod`; normal
  churn (EOF, 410 Gone, resource expired) is classified benign, mirroring
  client-go. A failure must be ONGOING to unready the pod: client-go retries a
  broken watch continuously, so a watch that stops reporting has recovered, and
  the failure is cleared. A delivered event cannot be the only recovery signal,
  because a relist of an unchanged ConfigMap delivers none (client-go drops sync
  notifications at `resync=0`). The first-failure clock still decides the grace,
  so continuous failures cannot hold the probes green. NOT covered: a watch that
  is alive but silently delivers nothing — that needs a periodic ground-truth
  GET, deliberately not built.
- The server starts before the Vault login so the bounded ignition retry (3m)
  reports live-but-not-ready instead of an unanswered port.

**Metrics** (`internal/metrics`, served at `/metrics` on `health_addr`):

- Shares the probe server. The manager runs one HTTP listener, so metrics need
  no address, port, flag, or env var of their own — and `health_addr=""`
  disables them along with the probes.
- Collectors are package-level on a private registry, not injected through an
  interface. The reconciler's `VaultOps`/`PodOps` interfaces exist so tests can
  run without a cluster; metrics need no such escape hatch.
- Vault token and ConfigMap watcher state are read from `health.State` at scrape
  time (`GaugeFunc`), not pushed, so the gauges cannot drift from the probes.
  `metrics.RegisterHealth` takes closures rather than a `*health.State`, which
  is what lets `health` import `metrics` without a cycle.
- A skipped pass keeps a working manager Ready but never opens the startup gate,
  so a rollout cannot go green on a ConfigMap that has never parsed.
- `result="skipped"` is a distinct bucket from `error`, and a skipped pass does
  not stamp `last_successful_reconcile_timestamp_seconds` — a ConfigMap that has
  been broken for an hour must not look like a healthy manager.
- The alerting label combinations are pre-seeded at zero so a new pod reads as
  zero errors rather than no data. No metric is labelled by pod name (unbounded
  cardinality, worthless history).
- A mount reconcile reports `MountResult{Changed, Reload}`: description and
  options drift is applied like version drift, but only a version or options
  move reloads the plugin (options reach the backend at initialization; a
  description does not), because a reload re-initializes the backend on every
  HA node.
- `vault_actions_total` counts writes that landed, not calls attempted: the
  `Ensure*` wrappers return `changed`, and `metrics.VaultActionIf` records only
  on `changed || err != nil`. Steady state is therefore flat, and a persistent
  rate is the signature of the idempotency bug class described in the
  version-prefix note. Counting the attempt instead puts three actions at a
  permanent non-zero rate and hides a real regression in that floor.

Command shape: `vault-plugin-manager serve` (long-running). A `reconcile` one-shot
subcommand is a nice-to-have for CI/debugging.

---

## 5. Vault ACL policy (documented in README)

The manager's Vault role maps to a policy granting:

```hcl
# Register / deregister / read plugins in the catalog (root-protected -> sudo)
path "sys/plugins/catalog/*" {
  capabilities = ["create", "read", "update", "delete", "list", "sudo"]
}

# Reload a plugin backend after (re)registration
path "sys/plugins/reload/backend" {
  capabilities = ["create", "update", "sudo"]
}

# Enable / tune / disable secret engine mounts
path "sys/mounts" {
  capabilities = ["read"]
}
path "sys/mounts/*" {
  capabilities = ["create", "read", "update", "delete"]
}

# Read/list auth-method mounts. REQUIRED even with ZERO managed auth mounts: the
# reconcile's prune pass (`ListManagedMounts`) lists both secret AND auth mounts
# every loop.
path "sys/auth" {
  capabilities = ["read"]
}
# Auth-method mount management — only needed for auth-type plugins.
path "sys/auth/*" {
  capabilities = ["create", "read", "update", "delete", "sudo"]
}

# Engine config, one stanza per engineConfigs entry. No read: vpm never reads it
# back (secret fields come back redacted), it compares against its own digest.
path "github/config" {
  capabilities = ["create", "update"]
}
```

Exact paths validated during implementation. Two non-obvious facts, both from Vault
ACL being **exact-match** (no implicit prefix nesting):

- `sys/auth` **read** is mandatory even for a secrets-only manager — `ListManagedMounts`
  calls `ListAuth` unconditionally every reconcile for the prune/drift pass.
- Narrowing `sys/mounts/*` to per-mount paths for least privilege must grant the
  `.../tune` subpath too (`sys/mounts/<name>` + `sys/mounts/<name>/tune`), because
  `TuneMount` on a `plugin_version` bump / config drift hits `.../tune`, which the
  bare mount path does not cover. See README "Least-privilege variant".

---

## 6. Kubernetes RBAC (manager ServiceAccount)

Namespaced `Role` (same namespace as Vault):

- `configmaps`: `get`, `list`, `watch`
- `pods`: `get`, `list` (discover Vault pods)
- `pods/exec`: `create` (copy binaries in)
- `secrets`: `get`, `update`, scoped by `resourceNames` to the state Secret
  (§3a). Always granted: vpm reads it every pass, so it can drop recorded digests
  even after the last `engineConfigs` entry is removed.
- `secrets`: `create`, only with `engineConfigs.enabled`, for the first write of
  the state Secret. Kubernetes cannot scope `create` by name, so this lets vpm
  create Secrets in the namespace, not read them.
- `secrets`: `get`, only with `engineConfigs.enabled`, scoped by `resourceNames`
  to `engineConfigs.secretNames`. A `secretRef` the chart does not list fails the
  pass with a 403 naming the Secret.

Plus the Vault-side k8s auth role binding this SA + namespace.

---

## 7. Helm chart (`chart/`)

Templates:
- `deployment.yaml` — 1 replica, `Recreate`, env/flags from values, resources, probes.
- `serviceaccount.yaml`
- `rbac.yaml` — Role + RoleBinding (namespaced).
- `configmap.yaml` — optional example/default plugins ConfigMap (toggle in values).
- `service.yaml` — ClusterIP exposing `/metrics` (toggle `metrics.enabled`); it
  exists only for scraping, since probes are called on the pod directly.
- `servicemonitor.yaml` — `monitoring.coreos.com/v1` ServiceMonitor (toggle
  `metrics.service_monitor.enabled`, default off). No CRD capability guard: a
  missing operator fails the install loudly rather than installing nothing.
- `_helpers.tpl`, `Chart.yaml`, `values.yaml`.

Key `values.yaml`: image repo/tag, `vault.addr`, `vault.authRole`, `vault.authMount`,
`vault.podSelector`, `plugin.dir`, `resyncInterval`, `pruneMode`, `configMap.name`,
resources, TLS settings, `health` (`enabled`, `port`, and the `livenessProbe`
/ `readinessProbe` maps, rendered verbatim so any probe field can be overridden),
`stateSecretName` (default `<fullname>-state`), `engineConfigs` (`enabled`,
`secretNames`; drives the Secret RBAC in §6), and `metrics` (`enabled`, plus a
`service_monitor` block). The `metrics` keys are
snake_case while the rest of the file is camelCase — deliberate, since converting
the existing keys would break every current override.

---

## 8. Package layout (proposed)

```
cmd/vault-plugin-manager/        # cobra root + serve subcommand
internal/args/                   # config struct (extends existing pattern)
internal/cmds/serve/             # wires informer + reconciler
internal/config/                 # ConfigMap YAML schema types + parse
internal/vault/                  # k8s-auth client, catalog, mounts, reload
internal/k8s/                    # pod discovery + exec-copy
internal/fetch/                  # PluginFetcher interface + http + oci impls
internal/reconcile/             # the reconcile loop
internal/health/                 # liveness/readiness probe server (also mounts /metrics)
internal/metrics/                # Prometheus collectors + handler
chart/                           # Helm chart
```

---

## 9. Resolved design points

- **OCI convention** — the binary is a **file inside the image rootfs** at a
  `source.path` given in the ConfigMap (configurable per plugin). Fetcher pulls
  the image (`go-containerregistry`), reads that path from the flattened rootfs,
  and treats it like any other binary from there on.
- **exec-copy transport** — **stream via `exec sh -c 'cat > file'`** (no `tar`
  dependency in the Vault image). Always **verify sha256 on the placed file**
  using the image's `/usr/bin/sha256sum` (confirmed available) before registering.
- **Token strategy** — **renew via a Vault lifetime watcher**; **re-login** when
  the token is non-renewable or renewal fails.

## 10. Open items / known limitations

- **Database-plugin mount handling** — catalog registration works; how the
  `database` secrets engine consumes a registered plugin is not yet wired.
- **Catalog pruning scope** — pruning deregisters a version (and, in `full` mode,
  removes its binary) only when that version was attached to a manager-owned
  mount that left the spec. A catalog entry registered without any mount, then
  removed, is not auto-deregistered (catalog entries carry no ownership marker,
  so blind deregistration would risk clobbering plugins we do not manage).
- **Log level / resync retune** — both are re-read from the ConfigMap each
  reconcile. Log level applies immediately (atomic-level logger); resync is
  driven by our own timer (informer resync is 0), so a changed `resyncInterval`
  takes effect on the next tick without a restart.
- **Engine config drift is not corrected.** vpm compares against its own record
  of what it wrote, not against Vault. A recreated mount is caught by its UUID,
  but a config edited in place in Vault stays edited until the spec or Secret
  content changes or its record is deleted from the state Secret. Secret fields read back redacted, so there is no
  live value to compare against.
- **Engine config crash window.** A crash between the Vault write and the state
  save rewrites the same body on the next pass (§3a "Ordering").
