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
6. **Prune** (per mode) — mounts/versions that are manager-owned but no longer in
   the ConfigMap:
   - `full`: disable mount → deregister version → `rm` binary on each pod.
   - `deregister`: disable mount → deregister version, **leave** binary on disk.
   - `never`: no-op (add/update only).

Reconcile is **idempotent** and **level-triggered** — every run drives live state
toward the ConfigMap regardless of what changed.

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
```

Notes:
- `catalog` may register multiple versions of the same plugin; each `mounts` entry
  pins the **active** version for that mount.
- `type: database` plugins register in the catalog but are consumed via the
  `database` secrets engine rather than a standalone mount (mount handling for
  database plugins TBD in implementation).

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

**Runtime tunables** — live in the watched ConfigMap's `settings:` block, re-read
each reconcile so they change without a redeploy:

| Setting | Default | Purpose |
|---------|---------|---------|
| `pruneMode` | `full` | `full` \| `deregister` \| `never` (see §2 step 6) |
| `resyncInterval` | `5m` | periodic drift reconcile |
| `logLevel` | `info` | `debug` \| `info` \| `warn` \| `error` |

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

# Enable / tune / disable auth method mounts (for auth-type plugins)
path "sys/auth" {
  capabilities = ["read"]
}
path "sys/auth/*" {
  capabilities = ["create", "read", "update", "delete", "sudo"]
}
```

(Exact paths validated during implementation; `sys/auth/*` typically needs `sudo`.)

---

## 6. Kubernetes RBAC (manager ServiceAccount)

Namespaced `Role` (same namespace as Vault):

- `configmaps`: `get`, `list`, `watch`
- `pods`: `get`, `list` (discover Vault pods)
- `pods/exec`: `create` (copy binaries in)

Plus the Vault-side k8s auth role binding this SA + namespace.

---

## 7. Helm chart (`chart/`)

Templates:
- `deployment.yaml` — 1 replica, `Recreate`, env/flags from values, resources, probes.
- `serviceaccount.yaml`
- `rbac.yaml` — Role + RoleBinding (namespaced).
- `configmap.yaml` — optional example/default plugins ConfigMap (toggle in values).
- `_helpers.tpl`, `Chart.yaml`, `values.yaml`.

Key `values.yaml`: image repo/tag, `vault.addr`, `vault.authRole`, `vault.authMount`,
`vault.podSelector`, `plugin.dir`, `resyncInterval`, `pruneMode`, `configMap.name`,
resources, TLS settings.

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
