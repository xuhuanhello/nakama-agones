# Local validation

Requirements: Docker, kubectl, Python 3.10+, Node 22+ and about 8 GB available to Docker. `scripts/bootstrap-tools.sh` downloads pinned k3d/Helm binaries and verifies the official published checksums. Alternatively set `K3D` and `HELM` to existing compatible binaries.

```sh
./scripts/bootstrap-tools.sh
python3 scripts/local_cluster.py up
node scripts/smoke.mjs
```

Only the dedicated `agones-nakama` k3d cluster is managed. The default kubeconfig is not changed. Kubernetes API 17443, Nakama API 17850, console 17851 and UDP 17770–17789 bind loopback. The databases run inside that cluster. Console development login is `admin` / `local-console-password`; the client server key is `local-server-key`. Generated operator/signing credentials and kubeconfig are ignored in `.local/` and are never printed by the scripts.

The runner exercises actual websocket matchmaking, persistent room state, actual Kubernetes GameServer/Secret creation, Agones Ready→Allocated, UDP signed admission and nonce rejection, multi-room placement/scale-out, cancellation, controller restart/resume and drain after result completion. `.local/smoke-result.json` records only safe check names and scope. It does not measure Unity/FishNet gameplay or production capacity.

```sh
# Inspect only this cluster; do not dump Secret data or full environment.
kubectl --kubeconfig .local/kubeconfig -n agones-games get gameservers
kubectl --kubeconfig .local/kubeconfig -n agones-control get pods
# Restore API forwarding after restarting Nakama.
python3 scripts/local_cluster.py forward
# Stop, preserving the local database and cluster containers.
python3 scripts/local_cluster.py stop
# Explicitly delete this local test cluster and its database when no longer needed.
python3 scripts/local_cluster.py reset
```

Do not expose this fixture to the LAN/internet. The local backend explicitly enables development HTTP for cluster callbacks; the production Unity bridge still requires HTTPS (except explicit loopback development mode). For real Unity local testing, use a trusted HTTPS control endpoint or a local TLS termination setup. Do not weaken production TLS validation to make the fixture work.

Source checks: `make test test-race`. PostgreSQL concurrency tests additionally require `AGONES_FLEET_TEST_DATABASE_URL` pointing at a dedicated test database. CI runs the complete isolated integration path on Linux amd64 and deletes only its own cluster afterward.
