# Deploying your own battle cluster

This is the production runbook, not an automatic cloud purchase script. Keep the existing PlayFlow deployment until DM acceptance succeeds on the new profile. Use a new pool/deployment identity and a separate test Nakama endpoint/database for the pilot.

## 1. Nodes and network

Start with one K3s server (control plane) and one Linux amd64 game worker; they may be separate VPSs. One server is not highly available. Three server nodes are needed for an embedded-etcd HA quorum. Reserve CPU/memory for the OS, K3s, Agones and monitoring. Benchmark the intended VPS model: vCPU count, burst allowance and peak network speed do not establish game capacity.

Follow the official [K3s installation](https://docs.k3s.io/quick-start) and [requirements](https://docs.k3s.io/installation/requirements). Pin a version supported by the selected Agones release. The local baseline is K3s v1.35.8+k3s1 / Agones 1.60.0.

Join workers with the private control endpoint and a securely supplied K3s join token. Do not commit it or expose it in a public tutorial command. Set each game node's reachable public `node-external-ip` and label eligible nodes `nakama-agones.io/game-node=true`. In Tencent networks this may be a NAT public address: verify return routing and a UDP handshake from outside, not just ICMP ping.

Keep API/kubelet/overlay communication private. Follow K3s's documented port matrix for your CNI. Public game clients need only the chosen UDP hostPort range (example 20000–20999). Do not expose Flannel VXLAN to the internet. Use private/VPN access when nodes do not share a VPC. Client traffic goes directly to the allocated node/UDP port; no paid cloud LoadBalancer is required by this design.

## 2. Agones and scoped controller permissions

Against the **explicit intended kubeconfig/context**:

```sh
kubectl --kubeconfig /secure/cluster.yaml apply -f deploy/kubernetes/rbac.yaml
helm upgrade --install agones https://agones.dev/chart/stable/agones-1.60.0.tgz   --kubeconfig /secure/cluster.yaml --namespace agones-system --create-namespace   --values deploy/agones.production.values.yaml --wait
```

The namespace Role permits only GameServer get/list/create/delete and Secret get/create/update/delete. Use the `agones-control/nakama-agones` service account for Nakama. The namespace is a trust boundary: label filtering prevents accidental adoption, but Kubernetes RBAC itself does not restrict writes by label. No cluster-admin token belongs in the plugin or dashboard.

Allocator and FleetAutoscaler are intentionally unused: the durable multi-room controller owns process scaling. Agones SDK lifecycle/health and Kubernetes scheduling remain active. Do not add another autoscaler that can delete the same occupied processes.

## 3. Nakama, database and secrets

Build/retrieve the independently released runtime and tools images. Pin immutable digests. Create two databases with separate least-privilege owners: Nakama's database and this plugin's fleet database. Run the official Nakama migration and the new `fleet-migrate` tool before starting the runtime. Production database transport must follow your TLS/CA policy; take tested backups.

`deploy/production.env.example` lists configuration. Store filled values in a Kubernetes Secret or a private secret manager, never a ConfigMap or Git. Use 32 random bytes encoded as unpadded base64url for the signing key, and at least 32 random characters for the admin token. The deployment's projected service-account token and CA work automatically in-cluster. External Nakama may instead use a private API URL and restricted rotating token/CA files.

Expose the Nakama client API and game-agent routes over trusted HTTPS. Restrict `/agones/fleet/v1/admin/*` at the reverse proxy/network as well as its bearer authentication. Configure rate limits and timeouts. Players receive only short-lived, user-specific admission tickets. All per-worker injected environment, including game-specific result credentials, is supplied via Secret references.

The companion Unity lifecycle bridge requires HTTPS for remote control. Ensure the game image trusts its CA. Configuring readiness requires successful transport binding, business initialization and external result persistence, not merely a running Pod.

## 4. Game image and rollout

Build the Unity Linux amd64 Dedicated Server with the companion package and a completed room host adapter. Its entrypoint starts the game; it should read `AGONES_FLEET_GAME_PORT`, capacity and identity from environment. Upload it to your registry. Set `AGONES_GAME_IMAGE` to an immutable digest and use `AGONES_IMAGE_PULL_SECRET` if private.

A pool binds deployment ID, image/settings, manual compatibility version and region. To change these, start a new pool/deployment and route compatible clients to it, then drain the old one. Do not alter a live persisted profile and erase state to force it to start. Existing PlayFlow matchmaking remains a separate application/profile until a deliberate client rollout.

## 5. Operations and scale

`GET /agones/fleet/v1/admin/status` provides safe room/process state; `POST /agones/fleet/v1/admin/drain` requests safe process removal. Headlamp can provide a general Kubernetes UI, but game-specific room metrics/operations need an additional dashboard. Deploy Prometheus/Grafana with restricted access and short initial retention.

Record frame p99, input→authoritative-shot p95/p99, ball-stop→ready p95/p99, RTT, simulation queue age, rooms/players/reservations, result/audit backlog, CPU throttling/steal, RSS and actual NIC bytes. Compare 1/2/4/8 rooms per process before increasing process/node limits. Scaling a container does not purchase another VPS or lower an active VPS monthly bill.

Before promotion: verify external UDP on each node, real DM match/reconnect/rematch/results, graceful rolling drain, process/node failure isolation, image pull failure, API outage, Nakama restart, token rotation and database restore. No successful local fixture run substitutes for those real-game/production checks.
