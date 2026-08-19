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
- **Runtime tunables** — `pruneMode`, `resyncInterval`, `logLevel`. These live in
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

### Runtime tunables (ConfigMap `settings:`)

| Setting | Default | Purpose |
|---------|---------|---------|
| `pruneMode` | `full` | removal behavior — see below |
| `resyncInterval` | `5m` | periodic full drift reconcile (Go duration) |
| `logLevel` | `info` | `debug` \| `info` \| `warn` \| `error` |

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
```

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

Bind the manager's ServiceAccount to a Vault role backed by this policy, e.g.:

```sh
vault write auth/kubernetes/role/vault-plugin-manager \
  bound_service_account_names=vault-plugin-manager \
  bound_service_account_namespaces=<namespace> \
  policies=vault-plugin-manager \
  ttl=1h
```

Vault must be started with `plugin_directory` set to the path in `PLUGIN_DIR`.

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
