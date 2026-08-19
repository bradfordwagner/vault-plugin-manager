# CLAUDE.md

Guidance for working in this repo. See `SPEC.md` for the full design and
`README.md` for user-facing docs (config, ACL policy, chart usage).

## What this is

`vault-plugin-manager` is a long-running Go server that runs in the **same
namespace as a HashiCorp Vault server** and reconciles Vault's plugins to a
Kubernetes **ConfigMap**. Given a declarative list of plugins + versions it:

1. fetches each plugin binary (HTTPS URL or OCI image) and verifies its sha256,
2. **exec-copies** the binary onto every Vault pod's `plugin_directory` (HA-aware)
   and re-verifies the on-disk checksum,
3. registers the version in Vault's plugin catalog,
4. enables / tunes / reloads the secret & auth engine mounts that consume it,
5. prunes anything that leaves the ConfigMap (configurable).

It authenticates to Vault via the **Kubernetes auth method** and watches the
ConfigMap with an informer plus a settings-driven resync.

## Commands

```sh
go build ./...        # build
go test ./...         # unit tests (no cluster; fakes + httptest)
go vet ./...
gofmt -w internal/    # format (also: make test)
helm lint ./chart
helm template x ./chart --namespace vault   # render manifests

# end-to-end: real Vault on a kind cluster (needs docker, kind, kubectl, helm)
test/e2e/run.sh 1.20.4         # any hashicorp/vault tag; KEEP=1 leaves the cluster up

# container image (root Dockerfile; scratch/alpine variants)
docker build --build-arg BASE_IMAGE=scratch     -t vpm:scratch .
docker build --build-arg BASE_IMAGE=alpine:3.22 -t vpm:alpine  .
```

Or via [Task](https://taskfile.dev): `task` (build), `task check` (build+vet+test),
`task test`, `task lint`, `task e2e -- 1.20.4`, `task image -- alpine:3.22`;
`task --list` shows all. `make test`/`make watch`/`make clean` also exist.
The binary entrypoint is `./cmd/vault-plugin-manager` (subcommand: `serve`).
Unit tests need nothing; `test/e2e/` needs a container runtime + kind.

## Architecture (package layout)

- `cmd/vault-plugin-manager/` — cobra root + `serve` subcommand; flag/env wiring.
- `internal/args/` — `ServeArgs` bootstrap config struct.
- `internal/config/` — watched ConfigMap schema (`settings` + `catalog` + `mounts`),
  parse + validate. Runtime tunables live here, not in flags.
- `internal/vault/` — Vault client: k8s-auth login with background renew/re-login
  (`client.go`), catalog (`catalog.go`), mounts (`mounts.go`), reload (`reload.go`).
- `internal/k8s/` — clientset (`client.go`), pod discovery + exec-copy transport
  (`pods.go`), ConfigMap informer (`informer.go`).
- `internal/fetch/` — the `Fetcher`: HTTPS + archive extraction (`http.go`), OCI
  via go-containerregistry (`oci.go`), sha256 verify (`fetch.go`).
- `internal/reconcile/` — the idempotent, level-triggered loop (`reconcile.go`)
  and the informer/timer `Runner` (`runner.go`).
- `internal/logging/` — shared zap logger with a runtime-settable atomic level.
- `chart/` — Helm chart (deployment, serviceaccount, RBAC, optional ConfigMap).
- `Dockerfile` (root) — multi-stage: builds with the owned Go builder, slices the
  static binary into a `scratch` or `alpine` image.
- `test/e2e/` — end-to-end harness (kind + real Vault matrix). `testplugin/` is a
  **separate Go module** (a real minimal Vault secrets engine) so the Vault SDK
  stays out of the manager's dependency graph.

## Conventions

- **Config split by lifecycle.** *Bootstrap* config (Vault addr, k8s auth role,
  which ConfigMap to watch, pod selector, plugin dir) comes from flags/env because
  it's needed before the ConfigMap can be read. *Runtime tunables* (`pruneMode`,
  `resyncInterval`, `logLevel`) live in the ConfigMap's `settings:` block and are
  re-read every reconcile — changeable without a redeploy.
- **flag_helper pattern.** Flags are registered with
  `github.com/bradfordwagner/go-util/flag_helper` (supports string/bool/int/
  Duration only). Convention: lowercase flag name (`vault_addr`) ↔ uppercase
  `mapstructure`/env key (`VAULT_ADDR`); viper case-folds them. It's a generic
  func — call `flag_helper.CreateFlag(...)` directly, don't alias it.
- **Logging.** Use `internal/logging.Log()` (not `go-util/log`) so `logLevel`
  from settings applies at runtime via the atomic level.
- **Reconciler is testable.** It depends on narrow `VaultOps` / `PodOps`
  interfaces (satisfied by the real clients) and the `fetch.Fetcher` interface,
  so `reconcile_test.go` drives it with fakes — no cluster or Vault required.
- **Vault version strings carry a leading `v`.** Register `1.0.0` and Vault
  reports it back as `v1.0.0` (on catalog reads and mount `plugin_version`).
  Always compare versions with the `v` trimmed — `vault.sameVersion`,
  `reconcile.nvKey`. Skipping this makes every idempotency check miss, so the
  manager re-registers + **reloads every plugin on every reconcile** (churn that
  makes mounts flap). This bug was caught by the e2e; keep it fixed.

## Key design decisions (don't relitigate without reason)

- **Binary delivery = exec-copy to all matching Vault pods.** Each pod has its
  own filesystem, so binaries stream in via `sh -c 'cat > file'` (no `tar`
  dependency), then `chmod 0755`, then sha256 re-verify against the placed file
  (the official Vault image has `/usr/bin/sha256sum`). Registration happens once
  via the Vault API.
- **Plugin filename carries the version:** `<name>-<version>` in `plugin_directory`,
  so multiple versions coexist and Vault's registration `command` is that name.
- **Ownership marker.** Mounts the manager creates carry a
  `managed-by=vault-plugin-manager` option. Pruning (`ListManagedMounts`) only
  ever touches marked mounts, never foreign ones.
- **Prune modes** (`full` | `deregister` | `never`) — documented in README and on
  the `config.PruneMode` constants. Catalog pruning only deregisters a version
  that was attached to a pruned managed mount and is no longer referenced.
- **Token strategy:** lifetime-watcher renew, re-login when non-renewable / on
  failure.
- **Reload uses global scope** so standby HA nodes pick up the new binary.
- **Vault ACL needs `sudo`.** `sys/plugins/catalog/*` and `sys/plugins/reload/backend`
  are root-protected — the manager's policy must include `sudo` (see README ACL).
- **OCI insecure registries.** `OCI_INSECURE` (flag/env) / chart `ociInsecure`
  lets the OCI fetcher pull from plain-HTTP / untrusted-TLS registries (e.g. the
  in-cluster registry the e2e uses). Off by default.

## Build & release

- **Images**: root `Dockerfile`, built with the owned Go builder
  (`ghcr.io/bradfordwagner/go-builder:1.26-ubuntu_noble`, `GOTOOLCHAIN=auto` so the
  builder's Go minor need not match go.mod) and sliced into a `scratch` or
  `alpine-3.x` final image (CI matrix). Runs as numeric non-root `65532:65532`,
  `ENTRYPOINT=[binary]`, `CMD=[serve]`, with a bundled CA cert for TLS. No CNB /
  goreleaser (removed).
- **Workflows**: `docker_branches.yml` builds both variants on branch pushes;
  `docker_tags.yml` builds + pushes them on tag; `helm_tags.yml` publishes the
  chart as an OCI artifact on tag. Both tag workflows fire on `push: tags`, so
  **image and chart release together**. `e2e.yml` runs the Vault-version matrix
  (latest patch of the three most-recent community minor lines) on PRs.
- The e2e uses its own self-contained `test/e2e/Dockerfile.manager`
  (golang + distroless), so local e2e runs don't need the private builder.

## Known limitations

See `SPEC.md` §10: database-plugin mount handling isn't wired; catalog entries
registered without a mount aren't auto-pruned (no ownership marker on the
catalog). The reconcile chain **is** validated end-to-end against real Vault by
`test/e2e/` (register → mount → write/read → prune, across the version matrix).
