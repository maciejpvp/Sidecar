# Running the mesh on Kubernetes

Manifests for a local cluster (kind, minikube, k3d — Kubernetes 1.29+ for native sidecars).

```bash
docker build -t sidecar:dev .
kind load docker-image sidecar:dev            # or: minikube image load sidecar:dev

kubectl apply -f deploy/k8s/controlplane.yaml
kubectl apply -f deploy/k8s/example.yaml

kubectl logs deploy/web -c app -f              # "hello from orders-svc" every 2s
kubectl scale deploy/orders-svc --replicas=4   # new pods register, web spreads over them
kubectl -n mesh port-forward deploy/sidecar-controlplane 15100 &
curl -s localhost:15100/v1/snapshot | jq       # what every sidecar is routing by
```

Adding a service to the mesh is the sidecar block from `example.yaml` in its pod template —
no control-plane change, no mesh.yaml entry (it gets `defaults`), no list of instances anywhere.

What each piece is for, and why it is shaped that way: [../docs/DESIGN.md §13](../docs/DESIGN.md#13-control-plane).
