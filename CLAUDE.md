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
gofmt -w .            # format (or: task fmt)
helm lint ./chart
helm template x ./chart --namespace vault   # render manifests

# end-to-end: real Vault on a kind cluster (needs docker, kind, kubectl, helm)
test/e2e/run.sh 2.1.1          # any hashicorp/vault tag; KEEP=1 leaves the cluster up

# container image (root Dockerfile; scratch/alpine variants)
docker build --build-arg BASE_IMAGE=scratch     -t vpm:scratch .
docker build --build-arg BASE_IMAGE=alpine:3.22 -t vpm:alpine  .
```

Task runner is [Task](https://taskfile.dev) (no Makefile): `task` (build),
`task check` (build+vet+test), `task test`, `task lint`, `task watch` (watchexec
dev loop), `task e2e -- 2.1.1`, `task image -- alpine:3.22`; `task --list` shows all.
The binary entrypoint is `./cmd/vault-plugin-manager` (subcommand: `serve`).
Unit tests need nothing; `test/e2e/` needs a container runtime + kind.

## Architecture (package layout)

- `cmd/vault-plugin-manager/` — cobra root + `serve` subcommand; flag/env wiring.
- `internal/args/` — `ServeArgs` bootstrap config struct.
- `internal/config/` — watched ConfigMap schema (`settings` + `catalog` + `mounts`),
  parse + validate (`parse.go`, `types.go`), and the spec differ used for change
  logging (`diff.go`). Runtime tunables live here, not in flags.
- `internal/vault/` — Vault client: k8s-auth login with background renew/re-login
  (`client.go`), catalog (`catalog.go`), mounts (`mounts.go`), reload (`reload.go`).
- `internal/k8s/` — clientset (`client.go`), pod discovery + exec-copy transport
  (`pods.go`), ConfigMap informer (`informer.go`, which returns the informer so
  the probes can interrogate it).
- `internal/fetch/` — the `Fetcher`: HTTPS + archive extraction (`http.go`), OCI
  via go-containerregistry (`oci.go`), sha256 verify (`fetch.go`).
- `internal/reconcile/` — the idempotent, level-triggered loop (`reconcile.go`)
  and the informer/timer `Runner` (`runner.go`).
- `internal/health/` — liveness/readiness probe server (`health.go`): a watchdog
  `State` the reconcile Runner heartbeats, served on `HEALTH_ADDR`. Its mux also
  mounts `/metrics`.
- `internal/metrics/` — Prometheus collectors + handler (`metrics.go`).
- `internal/logging/` — shared zap logger with a runtime-settable atomic level.
- `chart/` — Helm chart (deployment, serviceaccount, RBAC, optional ConfigMap,
  optional metrics Service + ServiceMonitor).
- `Dockerfile` (root) — multi-stage: builds with the owned Go builder, slices the
  static binary into a `scratch` or `alpine` image.
- `test/e2e/` — end-to-end harness (kind + real Vault matrix). `testplugin/` is a
  **separate Go module** (a real minimal Vault secrets engine) so the Vault SDK
  stays out of the manager's dependency graph.

## Conventions

- **Config split by lifecycle.** *Bootstrap* config (Vault addr, k8s auth role,
  which ConfigMap to watch, pod selector, plugin dir) comes from flags/env because
  it's needed before the ConfigMap can be read — as is `health_addr`, since the
  probes must answer before the ConfigMap has ever been read. *Runtime tunables*
  (`pruneMode`, `resyncInterval`, `logLevel`, `stallTimeout`,
  `tokenGracePeriod`, `tokenFailTimeout`, `watchGracePeriod`) live in the
  ConfigMap's `settings:` block and are re-read every reconcile — changeable
  without a redeploy.
- **flag_helper pattern.** Flags are registered with
  `github.com/bradfordwagner/go-util/flag_helper` (supports string/bool/int/
  Duration only). Convention: lowercase flag name (`vault_addr`) ↔ uppercase
  `mapstructure`/env key (`VAULT_ADDR`); viper case-folds them. It's a generic
  func — call `flag_helper.CreateFlag(...)` directly, don't alias it.
- **Logging.** Use `internal/logging.Log()` (not `go-util/log`) so `logLevel`
  from settings applies at runtime via the atomic level.
- **Metrics share the health port.** `/metrics` is mounted on the probe mux
  (`health.Handler`), so there is no `METRICS_ADDR` and `HEALTH_ADDR=""` kills
  both. Collectors are package-level on a private registry in `internal/metrics`
  — deliberately NOT injected through an interface like `VaultOps`/`PodOps`,
  which exist only so reconcile tests can run without a cluster. Token and
  watcher gauges are `GaugeFunc` closures over `health.State` (pull, not push),
  which is also what keeps `health` -> `metrics` from being an import cycle.
  Tests assert DELTAS: the collectors are package-level and cannot be reset.
- **Count writes, not attempts.** Every `Ensure*` returns `changed`; record it
  with `metrics.VaultActionIf` (records on `changed || err != nil`), never
  `metrics.VaultAction`, which is only for call sites reached solely when work is
  really happening (reload, and the prune loops, which `continue` past anything
  still desired). A counter placed above the `changed` branch counts attempts,
  which puts register/mount/role_upsert at a permanent non-zero rate, breaks the
  "flat at steady state" reading the metric exists for, and hides a real
  re-registration storm in its own floor. The fakes in `reconcile_test.go` model
  `changed=false` on a repeat, and `TestReconcileSteadyStateRecordsNoVaultActions`
  reconciles twice and requires the second pass to record nothing — keep it.
- **Roles are read-compare-write like the other Ensure\*.** `EnsureRole`
  (`internal/vault/roles.go`) used to write unconditionally, which was real Vault
  traffic and real audit-log entries on every resync. The comparison is the hard
  part: Vault normalizes on read-back (a `"5m"` TTL reads back as `300`, numbers
  arrive as `json.Number`, omitted fields come back as plugin defaults), so
  `roleUpToDate` compares ONLY the keys the spec declares, through the
  duration/number normalization in `sameRoleValue`. A `reflect.DeepEqual` here
  reports drift forever and just trades one always-on counter for another. A role
  Vault will not serve back (read fails/unsupported) falls back to writing.
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
- **Vault ACL least-privilege gotchas** (both from ACL paths being exact-match):
  (1) `sys/auth` **read** is required even with zero managed auth mounts —
  `ListManagedMounts` (`internal/vault/mounts.go`) calls `ListAuth` unconditionally
  every reconcile for the prune pass; drop it and the loop 403-crashloops.
  (2) Narrowing `sys/mounts/*` to per-mount paths must also grant `sys/mounts/<name>/tune`
  — `ensureSecretMount` calls `TuneMount` (`.../tune`) on version/config drift, which
  the bare `sys/mounts/<name>` path does not cover. See README "Least-privilege variant".
- **Liveness is a watchdog, not an echo.** The manager serves no traffic, so
  `/healthz` reports on the reconcile loop: the Runner calls `Heartbeat` with its
  next deadline before each wait (`resyncInterval + stallTimeout`) and before
  each pass (`stallTimeout`), and liveness 503s once that passes. Don't "fix" it
  into a static 200 — a loop wedged on a hung exec/fetch/Vault call is exactly
  the failure it exists to catch. `/readyz` is a sticky startup gate (true on the
  first clean pass) so rollouts gate on a real reconcile without flapping on a
  transient Vault error; a skipped pass (absent/invalid ConfigMap) counts as
  clean. The probe server starts **before** the Vault login so the bounded
  ignition retry reports live-but-not-ready, not a dead port.
- **Vault token health feeds both probes, graced twice.** The client's
  login/renew loop reports through `vault.TokenObserver` (`internal/vault/client.go`);
  `health.State` implements it. Readiness drops after `tokenGracePeriod` (2m) —
  a manager that cannot authenticate is not working — liveness only after
  `tokenFailTimeout` (15m), because restarting the pod does not fix a Vault that
  is down, it only re-runs the login the client already retries. Repeated
  failures must NOT restart the grace clock, or a login retrying every second
  holds the probes green forever (`TokenInvalid` guards this; there is a test).
- **A dead ConfigMap watcher is the quiet failure.** The Runner reconciles on its
  own timer from the informer cache, so a dead watcher looks perfectly healthy:
  loop ticking, reconciles succeeding on stale content, edits ignored. Hence
  `informer.IsStopped()` fails liveness with NO grace (nothing in-process
  restarts an informer), while a watch that errors but still relists fails only
  readiness, after `watchGracePeriod`. Benign churn (EOF, 410 Gone, resource
  expired) must stay classified benign in `benignWatchError` — client-go asks for
  a randomized 5-10m watch timeout, so counting those would fire the probe
  constantly. Any delivered event clears a recorded failure.
- **Change logging: diff first, then actions.** `config.Diff` (`internal/config/diff.go`)
  reports what moved in the ConfigMap and the Runner logs one Info line per
  change before reconciling; the reconciler's existing logs record the work. Diff
  keys MATCH the reconciler's keys (`catalogKey`/`nvKey` = `name@version`,
  `mountDiffKey`/`mountKey` = `type:path`) so a logged change maps to the work it
  causes — keep them in sync. The Runner tracks the last-logged spec so a change
  is reported once, not every resync. Role changes log which `data` keys moved,
  never the values (the plugin owns that schema).
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
