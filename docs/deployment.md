# Deploying your own battle cluster

This runbook covers a fresh installation and replacement of an existing fleet provider. For an existing Nakama deployment, follow the [in-place cutover](cutover.md): preserve its endpoint, database connection strings and account data while replacing the active provider. A separate endpoint or database is not required.

## 1. Nodes and network

Start with one K3s server (control plane) and one Linux amd64 game worker; they may be separate VPSs. One server is not highly available. Three server nodes are needed for an embedded-etcd HA quorum. Reserve CPU/memory for the OS, K3s, Agones and monitoring. Benchmark the intended VPS model: vCPU count, burst allowance and peak network speed do not establish game capacity.

Use the repeatable [node installation and join runbook](nodes.md). The pinned Linux amd64 baseline is K3s v1.35.8+k3s1 / Agones 1.60.0, also used locally. It creates role labels, protects the control node from game placement, reserves OS/K3s resources and configures log rotation.

Join workers with the private control endpoint and a securely supplied K3s join token. Do not commit it or expose it in a public tutorial command. Set each game node's reachable public `node-external-ip` and label eligible nodes `nakama-agones.io/game-node=true`. In Tencent networks this may be a NAT public address: verify return routing and a UDP handshake from outside, not just ICMP ping.

Keep API/kubelet/overlay communication private. Follow K3s's documented port matrix for your CNI. Public game clients need only the chosen UDP hostPort range (example 20000–20999). Do not expose Flannel VXLAN to the internet. Use private/VPN access when nodes do not share a VPC. Client traffic goes directly to the allocated node/UDP port; no paid cloud LoadBalancer is required by this design.

## 2. Agones and scoped controller permissions

Against the **explicit intended kubeconfig/context**:

```sh
kubectl --kubeconfig /secure/cluster.yaml apply -f deploy/kubernetes/rbac.yaml
helm upgrade --install agones https://agones.dev/chart/stable/agones-1.60.0.tgz   --kubeconfig /secure/cluster.yaml --namespace agones-system --create-namespace   --values deploy/agones.production.values.yaml --wait
```

The namespace Role permits only GameServer get/list/create/delete and Secret get/create/update/delete. Use the `agones-control/nakama-agones` service account for Nakama. The namespace is a trust boundary: label filtering prevents accidental adoption, but Kubernetes RBAC itself does not restrict writes by label. No cluster-admin token belongs in the plugin or dashboard.

Production Helm values hard-select `nakama-agones.io/role=control` and tolerate the control taint installed by the node script. Two controller/extensions replicas on one control VPS do not provide host-level HA. GameServer placement uses the separate game-node selector in the environment template.

Allocator and FleetAutoscaler are intentionally unused: the durable multi-room controller owns process scaling. Agones SDK lifecycle/health and Kubernetes scheduling remain active. Do not add another autoscaler that can delete the same occupied processes.

## 3. Nakama, database and secrets

Build/retrieve the independently released runtime and tools images. Pin immutable digests. For a fresh installation, create two databases with separate least-privilege owners: Nakama's database and this plugin's fleet database. For an in-place provider replacement, retain both existing connection strings and databases as described in the cutover runbook. Run the official Nakama migration and the new `fleet-migrate` tool before starting the runtime. Production database transport must follow your TLS/CA policy; take tested backups.

`deploy/production.env.example` lists configuration. Store filled values in a Kubernetes Secret or a private secret manager, never a ConfigMap or Git. Use 32 random bytes encoded as unpadded base64url for the signing key, and at least 32 random characters for the admin token. The deployment's projected service-account token and CA work automatically in-cluster. External Nakama may instead use a private API URL and restricted rotating token/CA files.

For external Nakama, follow [short-lived credential issuance and rotation](credentials.md), including directory mounts and a tested refresh transport. The token issuer helper does not automatically install its scheduler or delivery mechanism.

Expose the Nakama client API and game-agent routes over trusted HTTPS. Restrict `/agones/fleet/v1/admin/*` at the reverse proxy/network as well as its bearer authentication. Configure rate limits and timeouts. Players receive only short-lived, user-specific admission tickets. All per-worker injected environment, including game-specific result credentials, is supplied via Secret references.

The companion Unity lifecycle bridge requires HTTPS for remote control. Ensure the game image trusts its CA. Configuring readiness requires successful transport binding, business initialization and external result persistence, not merely a running Pod.

### Matchmaking latency and warm capacity

Pairing is performed by Nakama's built-in matchmaker. The Go runtime plugin validates region/version on queue admission, then handles the matched callback and reserves a room. Changing the runtime language does not change the native matchmaking interval.

For a two-player game, start with this explicit Nakama configuration and measure queue time separately from process startup and UDP connection time:

```yaml
matchmaker:
  interval_sec: 1
  rev_precision: true
  rev_threshold: 0
```

Nakama 3.41 defaults to a 15-second matchmaker interval. A 1-second interval reduces the polling wait when a compatible opponent is already queued; it does not guarantee an opponent or immediate room readiness. Retain bidirectional query checks to keep private load-test groups separate from ordinary players. The example client submits equal minimum/maximum counts (2/2), so `max_intervals` is not a delay for expanding player count. See [Nakama configuration](https://heroiclabs.com/docs/nakama/getting-started/configuration/).

On monthly or otherwise always-on VPSs, use `AGONES_FLEET_MIN_INSTANCES=1` to retain one ready Unity game process even with no players. Extra empty processes may still retire after `AGONES_FLEET_IDLE_SECONDS`. This minimum counts ready processes, including occupied ones; it is not a guarantee of an additional empty process or room at peak load. The controller recreates the minimum after failure when healthy node capacity is available; one host is not HA.

These values load at Nakama startup. Back up the configuration, confirm no active allocations, preserve player database/session settings, and recreate only the Nakama service to apply environment changes. Verify the effective matchmaker configuration, then observe the warm process beyond the idle timeout. Joining a new worker node still does not require a Nakama restart.

Process scale-in only removes game Pods. It does not shut down, release, buy or reduce the bill of a VPS. Future pay-as-you-go worker scaling requires a separate node provisioning controller with minimum-node policy and drain-before-release checks; it is not implemented by this plugin.

## 4. Game image and rollout

Build the Unity Linux amd64 Dedicated Server with the companion package and a completed room host adapter. Its entrypoint starts the game; it should read `AGONES_FLEET_GAME_PORT`, capacity and identity from environment. Upload it to your registry. Set `AGONES_GAME_IMAGE` to an immutable digest and use `AGONES_IMAGE_PULL_SECRET` if private.

A pool binds deployment ID, image/settings, manual compatibility version and region. To change these, start a new pool/deployment and route compatible clients to it, then drain the old one. Do not alter a live persisted profile and erase state to force it to start. When replacing PlayFlow in place, coordinate the provider/client contract switch through the cutover runbook and keep only one active standalone FleetManager.

## 5. Operations and scale

Install the included [Fleet console](console.md) for rooms, player seats, instance/node metrics and logs. It uses [SSH-only access](console-access.md); its [versioned API](console-api.md) supports scripts and AI diagnostics. The optional local broker enables graceful instance drain. Headlamp is not required; add it only when general Kubernetes editing is needed. Prometheus/Grafana are a separate option for longer metric history and alerting, not prerequisites for this deployment.

Record frame p99, input→authoritative-shot p95/p99, ball-stop→ready p95/p99, RTT, simulation queue age, rooms/players/reservations, result/audit backlog, CPU throttling/steal, RSS and actual NIC bytes. Compare 1/2/4/8 rooms per process before increasing process/node limits. Scaling a container does not purchase another VPS or lower an active VPS monthly bill.

New eligible worker nodes become available to the scheduler without restarting Nakama. The pool's `AGONES_FLEET_MAX_INSTANCES` remains its startup-loaded process budget; adding hardware does not raise that value. Still-pending GameServers can be scheduled on new nodes, while expired launch/allocation requests must be retried. See [node scaling limits](nodes.md#what-adding-a-node-changes).

Before promotion: verify external UDP on each node, real DM match/reconnect/rematch/results, graceful rolling drain, process/node failure isolation, image pull failure, API outage, Nakama restart, token rotation and database restore. No successful local fixture run substitutes for those real-game/production checks.
