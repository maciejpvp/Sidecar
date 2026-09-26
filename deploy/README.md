# Running the mesh on Kubernetes

Manifests for a local cluster (kind, minikube, k3d — Kubernetes 1.29+ for native sidecars).

```bash
docker build --target sidecar      -t sidecar:dev .
docker build --target controlplane -t sidecar-controlplane:dev .
kind load docker-image sidecar:dev sidecar-controlplane:dev   # or: minikube image load …

kubectl apply -f deploy/k8s/controlplane.yaml
kubectl apply -f deploy/k8s/example.yaml

kubectl logs deploy/web -c app -f              # "hello from orders-svc" every 2s
kubectl scale deploy/orders-svc --replicas=4   # new pods register, web spreads over them
kubectl -n mesh port-forward deploy/sidecar-controlplane 15100 &
curl -s localhost:15100/v1/snapshot | jq       # what every sidecar is routing by
```

Adding a service to the mesh is the sidecar block from `example.yaml` in its pod template —
no control-plane change, no mesh.yaml entry (it gets `defaults`), no list of instances anywhere.

## Automated check

```bash
deploy/e2e.sh          # kind cluster → build → deploy → check → delete cluster (~4 min)
deploy/e2e.sh --keep   # leave the cluster up; it is always kept on failure
deploy/e2e.sh --down   # delete it
```

It uses its own kubeconfig (`$TMPDIR/sidecar-e2e.kubeconfig`), never `~/.kube/config`, and
checks against pod IPs, the control plane's `/v1/snapshot` and each sidecar's `/snapshot`
(through the API server proxy — the images are distroless, there is nothing to exec into):

| Step | What it pins down |
|---|---|
| Self-registration | the control plane lists exactly the running pods, with policy from the ConfigMap |
| Routing | `web` reaches `orders-svc` by name through its sidecar; an unregistered name is 404 |
| Scale out 2 → 4 | new pods are registered and routed with no config change |
| Scale in 4 → 1 | removed pods leave every table in seconds, by deregistering — no `instance_expired` |
| Control plane restart | calls keep succeeding throughout; the new one rebuilds from heartbeats |
| Policy hot reload | a ConfigMap edit reaches the sidecars (kubelet sync, ~60–90s) with no restart |

What each piece is for, and why it is shaped that way: [../docs/DESIGN.md §13](../docs/DESIGN.md#13-control-plane).
