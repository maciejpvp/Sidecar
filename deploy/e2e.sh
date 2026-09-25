#!/usr/bin/env bash
# End-to-end test of the mesh on a real Kubernetes cluster (kind).
#
#   deploy/e2e.sh            # create cluster, build, deploy, check; delete the cluster on success
#   deploy/e2e.sh --keep     # leave the cluster up afterwards (it is always kept on failure)
#   deploy/e2e.sh --down     # delete the cluster and exit
#
# Needs docker, kind, kubectl and jq. Uses its own kubeconfig, never ~/.kube/config.
# Re-running reuses an existing cluster, so iterating costs a build and a rollout, not a cluster.
set -euo pipefail

CLUSTER=sidecar-e2e
ROOT=$(cd "$(dirname "$0")/.." && pwd)
export KUBECONFIG=${TMPDIR:-/tmp}/$CLUSTER.kubeconfig
PATH=$PATH:$(go env GOPATH 2>/dev/null)/bin   # `go install`ed kind
KEEP=0

case "${1:-}" in
  --keep) KEEP=1 ;;
  --down) kind delete cluster --name "$CLUSTER"; rm -f "$KUBECONFIG"; exit 0 ;;
  "") ;;
  *) echo "usage: $0 [--keep|--down]" >&2; exit 2 ;;
esac

for bin in docker kind kubectl jq; do
  command -v "$bin" >/dev/null || { echo "missing: $bin" >&2; exit 2; }
done

# ---- output ---------------------------------------------------------------------------------

STEP=0
step() { STEP=$((STEP + 1)); printf '\n\033[1m%d. %s\033[0m\n' "$STEP" "$*"; }
ok()   { printf '   \033[32m✓\033[0m %s\n' "$*"; }
info() { printf '   %s\n' "$*"; }

on_exit() {
  local rc=$?
  if [ $rc -ne 0 ]; then
    printf '\n\033[31mFAILED\033[0m at step %d. Cluster kept for inspection:\n' "$STEP"
    echo "   export KUBECONFIG=$KUBECONFIG"
    echo "   kubectl logs -n mesh deploy/sidecar-controlplane"
    echo "   kubectl logs deploy/web -c sidecar"
    echo "   $0 --down    # when done"
  elif [ $KEEP -eq 0 ]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    rm -f "$KUBECONFIG"
  else
    printf '\nCluster kept: export KUBECONFIG=%s   (%s --down to delete)\n' "$KUBECONFIG" "$0"
  fi
}
trap on_exit EXIT

die() { printf '   \033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

# eventually <seconds> <description> <command...>: retry every second until the command succeeds.
eventually() {
  local timeout=$1 what=$2; shift 2
  local deadline=$((SECONDS + timeout))
  until "$@" >/dev/null 2>&1; do
    [ $SECONDS -lt $deadline ] || die "not within ${timeout}s: $what"
    sleep 1
  done
}

# ---- views of the mesh ----------------------------------------------------------------------
# Both go through the API server's proxy, so no port-forward is needed. The control plane and
# the sidecar images are distroless: there is no shell to exec into.

cp_snapshot() {
  kubectl get --raw "/api/v1/namespaces/mesh/services/sidecar-controlplane:api/proxy/v1/snapshot"
}
sidecar_admin() { # <pod> <path>
  kubectl get --raw "/api/v1/namespaces/default/pods/$1:15020/proxy$2"
}
web_pod() { kubectl get pod -l app=web -o jsonpath='{.items[0].metadata.name}'; }

# Sorted "ip:5678" of every running orders-svc pod, one per line.
# Terminating pods are left out: they deregister on SIGTERM.
orders_pods() {
  kubectl get pod -l app=orders-svc -o json | jq -r '.items[]
    | select(.status.phase == "Running" and .metadata.deletionTimestamp == null and .status.podIP != null)
    | .status.podIP + ":5678"' | sort
}
# Sorted instances of a service, as the control plane hands them out.
cp_instances() { # <service>
  cp_snapshot | jq -r --arg s "$1" '.services[] | select(.name == $s) | .instances[]' | sort
}
# The control plane's instances of orders-svc are exactly the running pods, and there are $1.
orders_registered() { # <count>
  local pods; pods=$(orders_pods)
  [ "$(printf '%s\n' "$pods" | grep -c .)" -eq "$1" ] && [ "$pods" = "$(cp_instances orders-svc)" ]
}
web_registered() { [ "$(cp_instances web | grep -c .)" -eq 1 ]; }
# web's sidecar routes orders-svc to $1 instances.
web_sees() { # <count>
  [ "$(sidecar_admin "$(web_pod)" /snapshot |
    jq '.snapshot.services[] | select(.name == "orders-svc") | .instances | length')" -eq "$1" ]
}

# A call from web's app container, through its sidecar (http_proxy is set in the pod).
call() { # <service>
  kubectl exec "$(web_pod)" -c app -- wget -qO- -T 5 "http://$1/" 2>&1
}
calls_succeed() { # <n>
  local i out
  for i in $(seq "$1"); do
    out=$(call orders-svc) || die "call $i failed: $out"
    [ "$out" = "hello from orders-svc" ] || die "call $i: unexpected body: $out"
  done
}

# ---- 1. cluster and images ------------------------------------------------------------------

step "Cluster $CLUSTER"
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  kind export kubeconfig --name "$CLUSTER" >/dev/null 2>&1
  ok "reusing existing cluster"
else
  kind create cluster --name "$CLUSTER" --wait 120s
  ok "created"
fi
info "$(kubectl version -o json | jq -r '"server " + .serverVersion.gitVersion')"

step "Build and load images"
docker build -q --target sidecar      -t sidecar:dev              "$ROOT" >/dev/null
docker build -q --target controlplane -t sidecar-controlplane:dev "$ROOT" >/dev/null
kind load docker-image --name "$CLUSTER" sidecar:dev sidecar-controlplane:dev >/dev/null
ok "sidecar:dev, sidecar-controlplane:dev"

# A re-run starts from the manifests again, not from whatever the last run left behind.
kubectl delete -f "$ROOT/deploy/k8s/example.yaml" --ignore-not-found --wait >/dev/null
kubectl delete -f "$ROOT/deploy/k8s/controlplane.yaml" --ignore-not-found --wait >/dev/null

# ---- 2. deploy ------------------------------------------------------------------------------

step "Deploy the control plane and two services"
kubectl apply -f "$ROOT/deploy/k8s/controlplane.yaml" >/dev/null
kubectl -n mesh rollout status deploy/sidecar-controlplane --timeout=120s >/dev/null
ok "control plane running"
kubectl apply -f "$ROOT/deploy/k8s/example.yaml" >/dev/null
kubectl rollout status deploy/orders-svc --timeout=180s >/dev/null
kubectl rollout status deploy/web --timeout=180s >/dev/null
ok "orders-svc ×2, web ×1 — every sidecar passed /readyz, so each has a snapshot"

# ---- 3. discovery ---------------------------------------------------------------------------

step "Self-registration: the control plane lists exactly the running pods"
# Up to one lease TTL (15s) of warmup after a fresh control plane, then a heartbeat.
eventually 60 "control plane lists both orders-svc pods" orders_registered 2
eventually 30 "control plane lists web" web_registered
ok "orders-svc: $(cp_instances orders-svc | paste -sd' ')"
ok "web:        $(cp_instances web)"
[ "$(cp_snapshot | jq '.services[] | select(.name == "orders-svc") | .policy.timeout')" -eq 2000000000 ] ||
  die "orders-svc timeout is not the 2s from mesh.yaml"
ok "orders-svc policy from mesh.yaml: timeout 2s"

# ---- 4. routing -----------------------------------------------------------------------------

step "web → orders-svc by name, through the sidecar"
eventually 30 "web's sidecar sees both instances" web_sees 2
calls_succeed 10
ok "10/10 calls answered \"hello from orders-svc\""
# grep -c, not -q: with pipefail, -q closing the pipe early can fail the pipeline on a match.
[ "$(kubectl logs deploy/web -c app --since=60s | grep -c "hello from orders-svc")" -gt 0 ] ||
  die "web's own loop never got an answer"
ok "web's background loop is getting answers too"

step "A name nothing registers"
out=$(call no-such-svc) && die "expected a failure, got: $out"
grep -q "404" <<<"$out" || die "expected 404, got: $out"
ok "404 (no_route) — not a DNS error, not a hang"

# ---- 5. scale ------------------------------------------------------------------------------

step "Scale out: 2 → 4"
kubectl scale deploy/orders-svc --replicas=4 >/dev/null
kubectl rollout status deploy/orders-svc --timeout=120s >/dev/null
eventually 30 "control plane lists 4 orders-svc pods" orders_registered 4
eventually 30 "web's sidecar sees 4 instances" web_sees 4
calls_succeed 12
ok "4 registered, 4 routed, no config touched"

step "Scale in: 4 → 1, instances leave by deregistering, not by lease expiry"
since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
start=$SECONDS
kubectl scale deploy/orders-svc --replicas=1 >/dev/null
eventually 60 "control plane lists 1 orders-svc pod" orders_registered 1
eventually 30 "web's sidecar sees 1 instance" web_sees 1
ok "gone from every table in $((SECONDS - start))s"
# The terminating pods may still be dying; give any lease a chance to expire before looking.
sleep 16
expired=$(kubectl logs -n mesh deploy/sidecar-controlplane --since-time="$since" | grep -c instance_expired || true)
if [ "$expired" -gt 0 ]; then
  die "an instance expired instead of deregistering — graceful shutdown did not reach the control plane"
fi
ok "no instance_expired in the control plane log"
calls_succeed 5
ok "calls still succeed on the remaining instance"

# ---- 6. control plane restart ---------------------------------------------------------------

step "Control plane restart: routes survive, and the new one warms up before serving"
old_version=$(sidecar_admin "$(web_pod)" /snapshot | jq -r .snapshot.version)
kubectl -n mesh delete pod -l app=sidecar-controlplane --wait=false >/dev/null
# Traffic during the outage and the warmup: sidecars keep their last good table.
for _ in $(seq 10); do calls_succeed 1; sleep 1; done
ok "10 calls over 10s while the control plane was down or warming up"
kubectl -n mesh rollout status deploy/sidecar-controlplane --timeout=120s >/dev/null
eventually 60 "new control plane lists the one orders-svc pod" orders_registered 1
eventually 30 "new control plane lists web" web_registered
new_version() { [ "$(sidecar_admin "$(web_pod)" /snapshot | jq -r .snapshot.version)" != "$old_version" ]; }
eventually 60 "web's sidecar takes a snapshot from the new control plane" new_version
web_sees 1 || die "web's sidecar lost orders-svc after the restart"
calls_succeed 5
ok "new control plane rebuilt the registry from heartbeats; web moved to its snapshots"

# ---- 7. policy hot reload -------------------------------------------------------------------

step "Policy edit in the ConfigMap reaches the sidecars with no restart (kubelet sync: up to ~90s)"
restarts_before=$(kubectl get pod -l app=web -o jsonpath='{.items[0].status.initContainerStatuses[0].restartCount}')
kubectl -n mesh get configmap mesh-config -o json |
  jq '.data["mesh.yaml"] |= sub("timeout: 2s"; "timeout: 3s")' |
  kubectl apply -f - >/dev/null
web_timeout_3s() {
  [ "$(sidecar_admin "$(web_pod)" /snapshot |
    jq '.snapshot.services[] | select(.name == "orders-svc") | .policy.timeout')" -eq 3000000000 ]
}
start=$SECONDS
eventually 150 "web's sidecar has orders-svc timeout 3s" web_timeout_3s
ok "orders-svc timeout 2s → 3s in web's sidecar after $((SECONDS - start))s"
[ "$(kubectl get pod -l app=web -o jsonpath='{.items[0].status.initContainerStatuses[0].restartCount}')" = "$restarts_before" ] ||
  die "web's sidecar restarted"
ok "no restart"

printf '\n\033[32mPASS\033[0m — all checks on Kubernetes %s\n' \
  "$(kubectl version -o json | jq -r .serverVersion.gitVersion)"
