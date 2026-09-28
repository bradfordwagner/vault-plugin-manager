# vault-plugin-manager

A small Go server that runs in the **same namespace as a HashiCorp Vault server**
and keeps Vault's plugins reconciled to a Kubernetes **ConfigMap**. Given a
declarative list of plugins and versions, it:

1. Fetches each plugin binary (HTTPS URL or OCI image) and verifies its checksum.
2. **exec-copies** the binary onto every Vault pod's `plugin_directory`
   (HA-aware) and re-verifies the on-disk checksum.
3. Registers the version in Vault's plugin **catalog**.
4. Enables / tunes / reloads the secret & auth engine **mounts** that consume it.
5. **Prunes** anything that leaves the ConfigMap (configurable).

It watches the ConfigMap with a Kubernetes informer and re-reconciles on a
periodic resync to correct drift. See [SPEC.md](./SPEC.md) for the full design.

> Status: functionally complete — CLI, config, Vault client (k8s auth + renew),
> Kubernetes client (pod discovery + exec-copy), HTTPS/OCI fetchers, the
> reconcile loop, and the Helm chart are all in place. See SPEC.md § "Open items"
> for known limitations (database-plugin mounts, catalog-prune scope).

## Configuration

Configuration is split in two:

- **Bootstrap config** — how to reach Kubernetes and Vault and which ConfigMap to
  watch. Needed *before* the ConfigMap can be read, so it comes from flags/env
  on the `serve` command (flag is lowercase, env is uppercase, same name).
- **Runtime tunables** — `pruneMode`, `resyncInterval`, `logLevel`, `stallTimeout`. These live in
  the watched ConfigMap's `settings:` block and are re-read on every reconcile,
  so they can be changed by editing the ConfigMap without redeploying.

### Bootstrap config (flags / env)

| Env | Default | Purpose |
|-----|---------|---------|
| `VAULT_ADDR` | — | Vault API address |
| `VAULT_AUTH_MOUNT` | `kubernetes` | Vault k8s auth mount path |
| `VAULT_AUTH_ROLE` | — | Vault role bound to the manager's ServiceAccount |
| `VAULT_CA_CERT` / `VAULT_SKIP_VERIFY` | — | Vault TLS |
| `CONFIGMAP_NAME` | — | ConfigMap to watch |
| `CONFIGMAP_NAMESPACE` | own namespace | where the ConfigMap lives |
| `CONFIGMAP_KEY` | `plugins.yaml` | data key holding the spec |
| `VAULT_POD_SELECTOR` | `app.kubernetes.io/name=vault` | selector for Vault pods |
| `VAULT_NAMESPACE` | own namespace | where Vault pods run |
| `VAULT_CONTAINER` | `vault` | container to exec into |
| `PLUGIN_DIR` | `/vault/plugins` | Vault `plugin_directory` |
| `HEALTH_ADDR` | `:8080` | liveness/readiness listen address; empty disables |

### Runtime tunables (ConfigMap `settings:`)

| Setting | Default | Purpose |
|---------|---------|---------|
| `pruneMode` | `full` | removal behavior — see below |
| `resyncInterval` | `5m` | periodic full drift reconcile (Go duration) |
| `logLevel` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `stallTimeout` | `10m` | how long one reconcile pass may run before liveness fails |
| `tokenGracePeriod` | `2m` | how long the Vault token may be invalid before readiness drops |
| `tokenFailTimeout` | `15m` | how long the Vault token may be invalid before liveness drops |
| `watchGracePeriod` | `2m` | how long the ConfigMap watch may fail before readiness drops |

**`pruneMode`** controls what happens when a mount or plugin version the manager
owns is removed from the ConfigMap:

| Mode | Disable mount | Deregister version | Delete binary on pods | Use when |
|------|:-:|:-:|:-:|----------|
| `full` | ✅ | ✅ | ✅ | The ConfigMap is the single source of truth. |
| `deregister` | ✅ | ✅ | ❌ | You want fast rollback without re-fetching binaries. |
| `never` | ❌ | ❌ | ❌ | Add/update only; cleanup is manual. Safest. |

## ConfigMap schema

The watched ConfigMap holds a YAML document under `CONFIGMAP_KEY`
(`settings` + `catalog` + `mounts`):

```yaml
settings:                            # runtime tunables (all optional; defaults shown)
  pruneMode: full                    # full | deregister | never
  resyncInterval: 5m
  logLevel: info

catalog:
  - name: vault-plugin-secrets-foo   # catalog name
    type: secret                     # secret | auth | database
    version: "0.3.1"                 # version to register
    source:
      url: https://releases.example.com/foo_0.3.1_linux_amd64.zip  # one of url|image
      image: ghcr.io/org/vault-plugin-secrets-foo:0.3.1
      path: /plugin/foo              # OCI only: binary path inside the image rootfs
      sha256: "<optional expected checksum>"
      binary: foo                    # binary name inside an archive (defaults to name)

mounts:
  - path: foo                        # mount path
    plugin: vault-plugin-secrets-foo # references catalog[].name
    type: secret                     # secret | auth
    version: "0.3.1"                 # active version pinned to this mount
    config:
      description: "Foo secrets engine"
      options: {}

roles:                               # secret-engine role bodies to UPSERT (optional)
  - mount: foo                       # references a mounts[].path
    name: reader                     # role name (the last path segment)
    data:                            # written verbatim; the plugin owns the schema
      ttl: "1h"
  - mount: foo
    name: reader
    rolesPath: realm/example/roles   # optional; default `roles`
    data:
      ttl: "1h"
```

**`roles`** upserts each role at `<mount>/<rolesPath>/<name>`. `data` is written
verbatim to the plugin, which owns the schema — vpm only owns *placement*.

- **`rolesPath`** (optional, default `roles`) is the path segment(s) *between* the
  mount and the role name. The default reproduces the classic `<mount>/roles/<name>`
  layout. An override like `realm/<realm>/roles` places the role at
  `<mount>/realm/<realm>/roles/<name>`, for plugins that use a deeper, plugin-owned
  role hierarchy. It is trimmed of surrounding slashes; empty, `.`/`..`, or
  double-slash segments are rejected.
- Under `pruneMode: full`, a role under a *declared* `rolesPath` on a managed mount
  that is not listed here is deleted. **Limitation:** a `rolesPath` the ConfigMap
  never declares is never enumerated, so its stale roles are not pruned — vpm stays
  plugin-agnostic and cannot discover role hierarchies it was not told about.

## Health probes

The manager serves no traffic, so "healthy" is defined by its reconcile loop, not
by request handling. Two endpoints on `HEALTH_ADDR` (`:8080` by default) answer
`200` when the probed condition holds and `503` — with a JSON body explaining why
— when it does not:

| Endpoint | Probe | Semantics |
|----------|-------|-----------|
| `/healthz` | liveness | A **watchdog on the reconcile loop**, plus the Vault token and the ConfigMap watcher. Before each wait the loop declares when its next signal is due (`resyncInterval + stallTimeout` while idle, `stallTimeout` while a pass runs). A loop stuck on a hung exec, fetch, or Vault call misses that deadline. It also fails once the Vault token has been invalid for `tokenFailTimeout`, or the ConfigMap informer has stopped. |
| `/readyz` | readiness | A **startup gate**, plus the Vault token and the ConfigMap watch. It flips true on the first clean reconcile pass, so `helm --wait` / `kubectl rollout status` gates on the manager actually reconciling. It drops again while the Vault token has been invalid for longer than `tokenGracePeriod`, or the watch has been failing for longer than `watchGracePeriod`. Reconcile errors appear in the body's `lastError` but do not unready the pod, so a transient error doesn't flap the rollout. |

### Vault token health

A manager that can't authenticate to Vault can't do its job, so both probes watch
the token. State comes from the client's login/renew loop — authoritative for the
token lifecycle — and is graced twice over:

| Token invalid for | Readiness | Liveness | Why |
|---|:-:|:-:|---|
| `< tokenGracePeriod` (2m) | ✅ | ✅ | Absorbs a Vault restart or a brief renew failure without flapping the pod. |
| `> tokenGracePeriod` | ❌ | ✅ | The manager is not working; say so. Restarting it wouldn't help — the client is already retrying login on a backoff. |
| `> tokenFailTimeout` (15m) | ❌ | ❌ | Last resort: assume the in-process re-login loop is itself stuck and let Kubernetes restart the pod. |

A successful re-login clears both immediately. Repeated failures don't restart the
grace clock, so a login retrying every second can't hold the probes green. The
probe body carries `tokenValid`, `tokenInvalidFor`, and `tokenError`.

`tokenFailTimeout` must be `>=` `tokenGracePeriod` (validated on parse): liveness
has to outlast readiness, or the pod gets restarted before it's ever reported
unready.

### ConfigMap watcher health

The reconcile loop runs on its own timer and reconciles whatever the informer
cache holds, so a dead watcher is invisible from the outside: the loop keeps
ticking, reconciles stale content successfully, and every probe stays green while
ConfigMap edits are silently ignored. Two checks close that:

| Failure | Detected by | Effect |
|---|---|---|
| Informer stopped outright | `informer.IsStopped()`, polled per probe request | **Liveness fails immediately.** No grace — nothing in-process restarts an informer, so only a pod restart fixes it. |
| Watch erroring, still relisting | client-go's watch-error handler | **Readiness fails after `watchGracePeriod`.** Liveness is untouched: client-go usually relists its way out. |

Normal watch churn is not counted. client-go asks for a randomized 5-10m watch
timeout and relists when a resource version ages out, so `io.EOF`,
`ErrUnexpectedEOF`, `410 Gone`, and resource-expired errors are classified benign
(mirroring client-go's own `DefaultWatchErrorHandler`) and logged at debug. Any
delivered event — a real change or a relist — clears a recorded watch failure.

What this still does **not** catch: a watch that the API server considers alive
but which silently delivers nothing. Detecting that needs a periodic direct `GET`
of the ConfigMap compared against the cache, which is not implemented.

Notes:

- The probe server starts **before** the Vault login, which retries for up to 3
  minutes during cluster ignition. A pod waiting on a Vault role that hasn't
  landed yet therefore reports live-but-not-ready rather than an unanswered port.
- An absent, empty, or unparseable ConfigMap counts as a *clean* pass: that's the
  spec being wrong, not the manager being broken. The skip is logged; readiness
  is not withheld for it.
- `stallTimeout` must exceed the slowest legitimate pass — fetching a large
  plugin and exec-copying it to every Vault pod — or healthy managers get killed.

```console
$ kubectl exec -it deploy/vault-plugin-manager -- wget -qO- localhost:8080/readyz
{"live":true,"ready":true,"uptime":"12m30s","lastPass":"2026-09-24T18:02:11Z","tokenValid":true,"watcherRunning":true}

# Vault down for four minutes: not ready, still live, and the body says why.
{"live":true,"ready":false,"reason":"Vault token invalid for 4m2s (grace 2m0s)",
 "uptime":"31m","lastPass":"2026-09-24T18:28:40Z","lastError":"vault: reading mounts: connection refused",
 "tokenValid":false,"tokenInvalidFor":"4m2s","tokenError":"vault: kubernetes login: connection refused","watcherRunning":true}
```

## Change logging

Every reconcile logs **what changed in the ConfigMap** before the reconciler logs
**what it did about it**. Changes are keyed the way the reconciler keys them, so a
line maps to the work it causes: `name@version` for catalog entries (versions
coexist, so a bump reads as a remove plus an add), `type:path` for mounts, and
`mount/rolesPath/name` for roles.

```
INFO  configmap change: catalog foo@1.1.0 added: type=secret, url=https://…/foo.tar.gz  section=catalog key=foo@1.1.0 action=added
INFO  configmap change: mounts secret:foo changed: version 1.0.0 -> 1.1.0             section=mounts  key=secret:foo action=changed
INFO  reconciling configmap changes   trigger=configmap changes=2
INFO  copied plugin binary            plugin=foo version=1.1.0 pod=vault-0
INFO  registered plugin version       plugin=foo version=1.1.0
INFO  reconciled mount                mount=foo version=1.1.0
INFO  reloaded plugin                 plugin=foo
```

Each change is logged once, not on every resync: a pass that finds nothing new
logs at `debug` only. Section, key, and action are also emitted as structured
fields (stable strings — grep them). Role bodies are the plugin's schema, so a
role change reports *which keys* moved, not their values. The first spec after
startup — or after the ConfigMap reappears — reports every entry as `added`.

## Vault ACL policy

The manager authenticates via the Vault **Kubernetes auth method**. Its role must
map to a policy with these capabilities:

```hcl
# Register / deregister / read plugins in the catalog.
# The plugin catalog is a root-protected path, so it requires "sudo".
path "sys/plugins/catalog/*" {
  capabilities = ["create", "read", "update", "delete", "list", "sudo"]
}

# Reload a plugin backend after (re)registration
path "sys/plugins/reload/backend" {
  capabilities = ["create", "update", "sudo"]
}

# Enable / tune / disable secret engine mounts. The wildcard covers the mount path
# AND its `.../tune` subpath (see least-privilege note below — Vault ACL paths are
# exact-match, so a narrower policy must grant `.../tune` explicitly).
path "sys/mounts" {
  capabilities = ["read"]
}
path "sys/mounts/*" {
  capabilities = ["create", "read", "update", "delete"]
}

# Read/list auth-method mounts. REQUIRED even when the manager manages ZERO auth
# mounts: every reconcile enumerates ALL mounts (secret AND auth) for the
# managed-mounts/prune pass — `ListManagedMounts` calls both `ListMounts` (GET
# sys/mounts) and `ListAuth` (GET sys/auth) unconditionally. Drop this read and
# the reconcile loop 403-crashloops the controller.
path "sys/auth" {
  capabilities = ["read"]
}
# Enable / tune / disable auth method mounts. Only needed when the catalog declares
# an auth-type plugin; a secrets-only manager can omit this wildcard entirely.
path "sys/auth/*" {
  capabilities = ["create", "read", "update", "delete", "sudo"]
}
```

Bind the manager's ServiceAccount to a Vault role backed by this policy, e.g.:

```sh
vault write auth/kubernetes/role/vault-plugin-manager \
  bound_service_account_names=vault-plugin-manager \
  bound_service_account_namespaces=<namespace> \
  policies=vault-plugin-manager \
  ttl=1h
```

Vault must be started with `plugin_directory` set to the path in `PLUGIN_DIR`.

### Least-privilege variant

The wildcards above are convenient but broad — `sys/mounts/*` lets the manager
enable/tune/delete *any* secret engine, not just the ones it declares. To scope the
policy to exactly the mounts in your config, replace the `sys/mounts/*` wildcard
with an explicit **pair** of stanzas per managed mount:

```hcl
# For a managed secret mount at path "github":
path "sys/mounts/github" {
  capabilities = ["create", "read", "update", "delete"]
}
# SEPARATE exact-match path — the stanza above does NOT cover `.../tune`. The
# manager calls TuneMount on mount-config drift or a `plugin_version` bump
# (`ensureSecretMount` -> `TuneMountAllowNilWithContext`), so without this it 403s
# on the next version bump. A fresh enable needs no tune, so a first-install run
# won't surface it.
path "sys/mounts/github/tune" {
  capabilities = ["create", "read", "update"]
}
```

Two things bite here, both because **Vault ACL paths are exact-match** (no implicit
prefix nesting):

1. **Grant `sys/auth` read regardless.** Keep the `sys/auth` read stanza even for a
   secrets-only manager — the reconcile's prune pass lists auth mounts every loop
   (see the comment on it above). You only drop the `sys/auth/*` *management*
   wildcard.
2. **Pair every mount with its `.../tune` subpath.** Adding a new backend to the
   catalog + mounts config requires adding *both* `sys/mounts/<name>` and
   `sys/mounts/<name>/tune` (auth-type plugins: `sys/auth/<name>` +
   `sys/auth/<name>/tune`). Miss the `/tune` half and the manager enables the mount
   fine but 403s the first time it tunes it.

## Kubernetes RBAC

The manager's ServiceAccount needs, in the Vault namespace:

- `configmaps`: `get`, `list`, `watch`
- `pods`: `get`, `list`
- `pods/exec`: `create`

These are rendered by the Helm chart (`rbac.create=true`).

## Helm chart

The chart lives in [`chart/`](./chart). Install into Vault's namespace:

```sh
helm install vault-plugin-manager ./chart \
  --namespace vault \
  --set vault.addr=https://vault.vault.svc:8200 \
  --set vault.authRole=vault-plugin-manager \
  --set configMap.name=vault-plugins
```

See [`chart/values.yaml`](./chart/values.yaml) for all values.

Probes are on by default and fully overridable — the `health.livenessProbe` /
`health.readinessProbe` maps are rendered verbatim into the container, so any
field a Kubernetes probe accepts can be set:

```sh
helm upgrade vault-plugin-manager ./chart \
  --set health.port=9090 \
  --set health.livenessProbe.periodSeconds=60 \
  --set health.livenessProbe.failureThreshold=5
```

`health.enabled=false` drops both probes, the container port, and the probe
server itself (`HEALTH_ADDR=""`).
