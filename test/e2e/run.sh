#!/usr/bin/env bash
# End-to-end test for vault-plugin-manager against a real Vault + kind cluster.
#
# It stands up a kind cluster, a dev Vault, an in-cluster registry and HTTP file
# server, installs the manager via the Helm chart with a ConfigMap declaring two
# plugins (one HTTPS source, one OCI source), and asserts the full chain:
# exec-copy -> register -> enable mount -> write/read a secret, then prune.
#
# Usage:   test/e2e/run.sh [VAULT_VERSION]
# Env:     VAULT_VERSION (default 1.18.5), KIND_CLUSTER, KEEP=1 (don't tear down)
set -euo pipefail

VAULT_VERSION="${1:-${VAULT_VERSION:-1.18.5}}"
VAULT_IMAGE="hashicorp/vault:${VAULT_VERSION}"
CLUSTER="${KIND_CLUSTER:-vpm-e2e}"
NS=e2e
MANAGER_IMAGE="vault-plugin-manager:e2e"
PLUGIN_IMAGE="e2eplugin:e2e"
MANAGER_DEPLOY="vpm-vault-plugin-manager"
KEEP="${KEEP:-0}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

log() { echo -e "\n\033[1;34m==> $*\033[0m"; }

# ensure_crane prints the path to a crane binary, downloading one if needed.
# crane runs as a host process, so it reaches the registry port-forward directly
# even when the docker daemon lives in a VM (Docker Desktop / Rancher Desktop).
ensure_crane() {
  if command -v crane >/dev/null 2>&1; then command -v crane; return; fi
  local ver=v0.20.2 tmp
  tmp="$(mktemp -d)"
  echo "installing crane ${ver}" >&2
  curl -sSL "https://github.com/google/go-containerregistry/releases/download/${ver}/go-containerregistry_$(uname -s)_$(uname -m).tar.gz" \
    | tar -xz -C "$tmp" crane
  echo "${tmp}/crane"
}

dump_diagnostics() {
  echo "::group::diagnostics"
  kubectl -n "$NS" get pods -o wide || true
  kubectl -n "$NS" describe pods || true
  echo "--- manager logs ---";  kubectl -n "$NS" logs "deploy/${MANAGER_DEPLOY}" --tail=200 || true
  echo "--- vault logs ---";    kubectl -n "$NS" logs deploy/vault --tail=100 || true
  echo "::endgroup::"
}

cleanup() {
  local rc=$?
  [[ $rc -ne 0 ]] && dump_diagnostics
  kubectl delete clusterrolebinding vault-e2e-auth-delegator --ignore-not-found >/dev/null 2>&1 || true
  if [[ "$KEEP" == "1" ]]; then
    echo "KEEP=1: leaving cluster '$CLUSTER' for inspection"
  elif [[ "${CREATED_CLUSTER:-0}" == "1" ]]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  else
    echo "reused pre-existing cluster '$CLUSTER'; leaving it"
  fi
  exit $rc
}
trap cleanup EXIT

apply_manifest() { # file, then sed substitutions applied via stdin pipeline
  sed -e "s|__VAULT_IMAGE__|${VAULT_IMAGE}|g" \
      -e "s|__PLUGIN_IMAGE__|${PLUGIN_IMAGE}|g" "$1" | kubectl apply -f -
}

vexec() { # run a shell snippet inside the vault pod with root creds
  kubectl -n "$NS" exec -i "$VPOD" -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root sh -c "$1"
}

retry() { # retry <timeout_s> <desc> <cmd...>
  local timeout="$1" desc="$2"; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
    if (( $(date +%s) >= deadline )); then
      echo "TIMEOUT waiting for: $desc"; return 1
    fi
    sleep 3
  done
  echo "ok: $desc"
}

assert_no_restarts() { # $1 = context label
  local restarts
  restarts="$(kubectl -n "$NS" get pod -l app.kubernetes.io/name=vault-plugin-manager \
    --field-selector=status.phase=Running -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}')"
  if [[ "${restarts:-0}" != "0" ]]; then
    echo "FAIL: manager restarted ${restarts}x by '$1'"
    kubectl -n "$NS" describe pod -l app.kubernetes.io/name=vault-plugin-manager | sed -n '/Last State/,/Restart Count/p'
    return 1
  fi
  echo "ok: manager has not restarted ($1)"
}

configmap_yaml() { # $1 = full|pruned ; emits the ConfigMap
  local oci_block=""
  local oci_mount=""
  if [[ "$1" == "full" ]]; then
    oci_block="
      - name: testplugin-oci
        type: secret
        version: \"1.0.0\"
        source:
          image: registry:5000/e2eplugin:1
          path: /plugin/foo
          sha256: \"${SHA}\""
    oci_mount="
      - path: e2e-oci
        plugin: testplugin-oci
        type: secret
        version: \"1.0.0\""
  fi
  cat <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: vault-plugins
  namespace: ${NS}
data:
  plugins.yaml: |
    settings:
      pruneMode: full
      resyncInterval: 15s
      logLevel: debug
    catalog:
      - name: testplugin-http
        type: secret
        version: "1.0.0"
        source:
          url: http://plugin-http:8080/foo
          binary: foo
          sha256: "${SHA}"${oci_block}
    mounts:
      - path: e2e-http
        plugin: testplugin-http
        type: secret
        version: "1.0.0"${oci_mount}
EOF
}

##### 1. cluster #####
log "Vault ${VAULT_VERSION} — kind cluster '${CLUSTER}'"
CREATED_CLUSTER=0
if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --wait 120s
  CREATED_CLUSTER=1
fi
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f -

##### 2. build + load images #####
log "Building images"
docker build -f test/e2e/Dockerfile.manager -t "$MANAGER_IMAGE" .
docker build -t "$PLUGIN_IMAGE" test/e2e/testplugin
kind load docker-image "$MANAGER_IMAGE" --name "$CLUSTER"
kind load docker-image "$PLUGIN_IMAGE" --name "$CLUSTER"

##### 3. infra: vault, registry, http server #####
log "Deploying Vault, registry, plugin HTTP server"
apply_manifest test/e2e/manifests/vault.yaml
kubectl apply -f test/e2e/manifests/registry.yaml
apply_manifest test/e2e/manifests/plugin-http.yaml
kubectl -n "$NS" rollout status deploy/vault --timeout=180s
kubectl -n "$NS" rollout status deploy/registry --timeout=120s
kubectl -n "$NS" rollout status deploy/plugin-http --timeout=120s

##### 4. push plugin image into the in-cluster registry #####
log "Pushing plugin image to in-cluster registry"
kubectl -n "$NS" port-forward --address 127.0.0.1 svc/registry 5000:5000 >/tmp/vpm-pf.log 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do curl -sf http://127.0.0.1:5000/v2/ >/dev/null 2>&1 && break; sleep 1; done
curl -sf http://127.0.0.1:5000/v2/ >/dev/null 2>&1 || { echo "registry not reachable via port-forward"; kill "$PF_PID" 2>/dev/null || true; exit 1; }
CRANE="$(ensure_crane)"
TARBALL="$(mktemp -d)/plugin.tar"
docker save "$PLUGIN_IMAGE" -o "$TARBALL"
"$CRANE" --insecure push "$TARBALL" 127.0.0.1:5000/e2eplugin:1
kill "$PF_PID" >/dev/null 2>&1 || true

##### 5. configure Vault: k8s auth + policy + role #####
log "Configuring Vault (kubernetes auth, policy, role)"
VPOD="$(kubectl -n "$NS" get pod -l app=vault -o jsonpath='{.items[0].metadata.name}')"
kubectl -n "$NS" exec -i "$VPOD" -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root sh <<'EOSH'
set -e
vault auth enable kubernetes 2>/dev/null || true
vault write auth/kubernetes/config \
  kubernetes_host=https://kubernetes.default.svc \
  disable_iss_validation=true
cat > /tmp/policy.hcl <<'EOF'
path "sys/plugins/catalog/*"      { capabilities = ["create","read","update","delete","list","sudo"] }
path "sys/plugins/reload/backend" { capabilities = ["create","update","sudo"] }
path "sys/mounts"                 { capabilities = ["read"] }
path "sys/mounts/*"               { capabilities = ["create","read","update","delete"] }
path "sys/auth"                   { capabilities = ["read"] }
path "sys/auth/*"                 { capabilities = ["create","read","update","delete","sudo"] }
EOF
vault policy write vault-plugin-manager /tmp/policy.hcl
vault write auth/kubernetes/role/vault-plugin-manager \
  bound_service_account_names=vault-plugin-manager \
  bound_service_account_namespaces=e2e \
  policies=vault-plugin-manager ttl=1h
EOSH

##### 6. compute plugin sha + apply the plugins ConfigMap #####
log "Computing plugin checksum and applying ConfigMap"
# Ensure the HTTP server serves the freshly built binary (matters on reused clusters).
kubectl -n "$NS" rollout restart deploy/plugin-http
kubectl -n "$NS" rollout status deploy/plugin-http --timeout=120s
SHA="$(kubectl -n "$NS" exec deploy/plugin-http -- sha256sum /www/foo | awk '{print $1}')"
echo "plugin sha256=${SHA}"
configmap_yaml full | kubectl apply -f -

##### 7. install the manager via the Helm chart #####
log "Installing manager chart"
helm upgrade --install vpm ./chart -n "$NS" -f test/e2e/values.e2e.yaml
# Force fresh pods so a reused cluster picks up a rebuilt image on the same tag.
kubectl -n "$NS" rollout restart "deploy/${MANAGER_DEPLOY}"
# rollout status now gates on the readiness probe, which only flips true after
# the manager's first clean reconcile pass.
kubectl -n "$NS" rollout status "deploy/${MANAGER_DEPLOY}" --timeout=180s

# Prove both probe endpoints actually answer 200. The manager image has no
# shell, so the request goes from the vault pod's busybox wget (-q exits
# non-zero on the 503 an unhealthy manager returns).
MGR_IP="$(kubectl -n "$NS" get pod -l app.kubernetes.io/name=vault-plugin-manager \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].status.podIP}')"
retry 60 "liveness probe 200"  vexec "wget -qO- http://${MGR_IP}:8080/healthz"
retry 60 "readiness probe 200" vexec "wget -qO- http://${MGR_IP}:8080/readyz"
# /metrics rides the same listener; assert it is routed and carries our series.
retry 60 "metrics endpoint serves vpm_ series" vexec \
  "wget -qO- http://${MGR_IP}:8080/metrics | grep -q '^vpm_build_info'"
# The Service is what a ServiceMonitor would select, so a wrong port or
# targetPort only shows up by scraping through it rather than the pod IP.
retry 60 "metrics Service routes to the endpoint" vexec \
  "wget -qO- http://${MANAGER_DEPLOY}-metrics:8080/metrics | grep -q '^vpm_build_info'"

# A restart (OOM, crash) would reset the manager's view of the ConfigMap and
# silently invalidate the change-log assertion at the end of this run, so fail
# loudly here instead.
assert_no_restarts "after install"

##### 8. assert the full chain #####
log "Asserting plugins registered + mounts working"
retry 120 "http plugin registered"  vexec 'vault plugin info -version=1.0.0 secret testplugin-http'
retry 120 "oci plugin registered"   vexec 'vault plugin info -version=1.0.0 secret testplugin-oci'
retry 120 "http mount enabled"      bash -c "kubectl -n $NS exec -i $VPOD -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root vault secrets list | grep -q '^e2e-http/'"
retry 120 "oci mount enabled"       bash -c "kubectl -n $NS exec -i $VPOD -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root vault secrets list | grep -q '^e2e-oci/'"

log "Writing and reading secrets through the plugins"
vexec 'vault write e2e-http/foo value=hello-http'
vexec 'vault write e2e-oci/foo  value=hello-oci'
got_http="$(vexec 'vault read -field=value e2e-http/foo')"
got_oci="$(vexec 'vault read -field=value e2e-oci/foo')"
[[ "$got_http" == "hello-http" ]] || { echo "http readback mismatch: '$got_http'"; exit 1; }
[[ "$got_oci"  == "hello-oci"  ]] || { echo "oci readback mismatch: '$got_oci'"; exit 1; }
echo "ok: read back hello-http and hello-oci"

##### 9. prune: drop the OCI entry, expect cleanup #####
log "Testing prune (removing the OCI plugin from the ConfigMap)"
configmap_yaml pruned | kubectl apply -f -
retry 120 "oci mount pruned" bash -c "! kubectl -n $NS exec -i $VPOD -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root vault secrets list | grep -q '^e2e-oci/'"
retry 60  "oci plugin deregistered" bash -c "! kubectl -n $NS exec -i $VPOD -- env VAULT_ADDR=http://127.0.0.1:8200 VAULT_TOKEN=root vault plugin info -version=1.0.0 secret testplugin-oci"
# http mount must survive the prune.
vexec 'vault secrets list | grep -q "^e2e-http/"'
echo "ok: OCI pruned, HTTP mount retained"

# The change log must name WHICH part of the ConfigMap moved, not just that it
# changed; the reconciler's own action logs then follow. This only holds for a
# process that observed BOTH specs — a restart in between resets the diff base
# to "nothing seen yet", so check that first.
assert_no_restarts "after prune"
retry 60 "configmap change logged with the removed mount" bash -c \
  "kubectl -n $NS logs deploy/${MANAGER_DEPLOY} --tail=-1 | grep -q 'configmap change: mounts secret:e2e-oci removed'"

##### 10. metrics reflect the work this run actually did #####
# Instrumentation that compiles but is wired to the wrong branch reads zero
# forever, and no unit test can tell the difference. This run copied binaries,
# registered plugins, and pruned a mount, so those counters must be non-zero.
log "Checking metrics recorded the reconcile work"
# Sums every sample line containing the given FIXED string (HELP/TYPE comments
# stripped first, so a bare metric name matches only real samples).
metrics_value() {
  vexec "wget -qO- http://${MGR_IP}:8080/metrics" \
    | grep -v '^#' | grep -F -- "$1" \
    | awk '{sum += $NF} END {printf "%d", sum+0}'
}
for series in \
  'vpm_plugin_binary_copies_total{' \
  'vpm_vault_actions_total{action="register",result="success"}' \
  'vpm_vault_actions_total{action="unmount",result="success"}' \
  'vpm_reconcile_total{result="success"'
do
  got="$(metrics_value "$series")"
  [[ "$got" -gt 0 ]] || { echo "metric not recorded (still 0): $series"; exit 1; }
  echo "ok: ${series} = ${got}"
done
# A skipped or failed pass must never stamp the freshness gauge.
fresh="$(metrics_value 'vpm_last_successful_reconcile_timestamp_seconds')"
[[ "$fresh" -gt 0 ]] || { echo "freshness gauge never stamped"; exit 1; }
echo "ok: freshness gauge stamped"

log "E2E PASSED for Vault ${VAULT_VERSION}"
