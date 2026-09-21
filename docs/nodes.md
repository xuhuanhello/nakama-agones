# Installing and joining VPS nodes

The pinned baseline is **K3s v1.35.8+k3s1 (Kubernetes 1.35.8) and Agones 1.60.0**. Kubernetes 1.35 is in the [Agones 1.60 compatibility matrix](https://agones.dev/site/docs/installation/); the [K3s release](https://github.com/k3s-io/k3s/releases/tag/v1.35.8%2Bk3s1) pins its embedded components. Version upgrades are a separate maintenance procedure.

`scripts/k3s_node.py` installs a new control node or joins a game worker to an existing private cluster. It does not purchase machines, modify cloud security groups, install Agones, or start Nakama. Linux amd64, systemd and Python 3.9+ are required. A matching rerun preserves service identity and does not restart an active K3s service. An existing unowned Kubernetes installation or changed profile is rejected.

## Before installing

Use a dedicated 2 vCPU / 4 GiB control VPS and at least 2 vCPU / 2 GiB per worker. This baseline starts a **single control node with SQLite**, not a highly available cluster. Back up its datastore, K3s server token and encryption configuration; test restore separately. Moving to three control nodes with embedded etcd requires an explicit migration. See [datastores](https://docs.k3s.io/datastore) and [backup/restore](https://docs.k3s.io/datastore/backup-restore).

On Debian/Ubuntu, install prerequisites as root:

```sh
apt-get update
apt-get install -y python3 ca-certificates curl iproute2 logrotate
```

Verify the private IP/interface with `ip -brief address`; disable active swap explicitly before installation. The script uses K3s's bundled userspace tools, including iptables. Existing Docker is left installed; K3s uses its own containerd and consumes separate disk/resources.

Configure the cloud security group and host firewall first. Keep these flows source-restricted to the intended private peers:

| Destination | Source | Allowed traffic |
| --- | --- | --- |
| Control | Workers and the external Nakama host | TCP 6443 |
| All cluster nodes | Other cluster nodes | UDP 8472, TCP 10250 |
| Private image registry, if used | Nodes that pull its images | Its configured HTTPS port, e.g. TCP 5443 |
| Game workers | Game clients | UDP 20000–20999 |
| SSH | Operator/automation addresses | TCP 22 |

A node can be `Ready` while cross-node Pod networking is broken. Check the protocol selector as well as the port: `8472` must be **UDP** on both control and worker firewalls. Verify DNS from a game Pod and a real public UDP session before accepting a worker. A successful SSH connection or ICMP ping is insufficient.

When adding a machine, extend any per-address rules on the control node, existing workers and private registry to include the new private peer. Preserve the host's CNI forwarding rules as well as its input rules, especially if Docker is also installed. The node script does not perform this firewall enrollment.

The port range must match the Agones Helm values. The [K3s network requirements](https://docs.k3s.io/installation/requirements) explain the overlay and kubelet flows. **Binding the API to a private IP does not block a cloud public-IP NAT mapping.** Confirm public 6443/8472/10250 are blocked. Use a private VPN between networks; this script requires RFC1918 IPv4 for the cluster endpoint.

## Optional private image registry

Before the first install on each node that needs private images, securely place two root-owned `0600` files outside the checkout: a registry JSON file and the PEM CA certificate. Example shape, with your own endpoint and independent read-only account:

```json
{
  "configs": {
    "10.0.0.10:5443": {
      "auth": {"username": "REPLACE", "password": "REPLACE"},
      "tls": {"ca_file": "/etc/rancher/k3s/private-registry-ca.crt"}
    }
  }
}
```

Append `--registry-config-file /root/registry.json --registry-ca-file /root/registry-ca.crt` to both the plan and installation commands. The script writes `registries.yaml` and the CA under `/etc/rancher/k3s/` before starting K3s. It rejects insecure TLS, HTTP mirrors, wildcard registries, token/CA drift and missing managed credentials. Plans and errors never show registry auth. Do not commit a filled JSON file or issue registry-admin credentials to workers. Registry rotation needs a deliberate per-node maintenance procedure; it is not part of an idempotent rerun.

## Install the control node

Run from this repository. Replace example identity/network values; keep them unchanged on reruns. Review with `--plan`, then run the same command without that flag:

```sh
python3 scripts/k3s_node.py manager \
  --cluster-id example-us --node-name control-01 \
  --private-ip 10.0.0.2 --interface eth0 --plan
```

The script downloads the pinned K3s binary from its official release and verifies its SHA256, then runs the version-pinned installer with an explicit config file and clean environment. Config and ownership records are stored under `/etc/rancher/k3s/` and `/etc/nakama-agones/`.

After installation, check readiness with the explicit private endpoint:

```sh
k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml \
  --server https://10.0.0.2:6443 wait --for=condition=Ready node/control-01 --timeout=180s
python3 scripts/k3s_node.py export-join-token --output /root/worker-join.token
```

This exports the separate **agent-only secure token**, including the cluster CA fingerprint, to a `0600` file. Transfer that file to the new worker using authenticated SSH with a verified host key. Never distribute `/var/lib/rancher/k3s/server/token`, which also grants server access. The [K3s token model](https://docs.k3s.io/cli/token) distinguishes these credentials; this script deliberately does not accept a short password or server token for worker joins.

## Join each game worker

Securely copy the same script to the new machine. Give each machine a unique node name and its own private IP and reachable public IPv4. `PUBLIC_IPV4` below must be replaced:

```sh
python3 scripts/k3s_node.py worker \
  --cluster-id example-us --node-name game-01 \
  --private-ip 10.0.0.3 --external-ip PUBLIC_IPV4 --interface eth0 \
  --server https://10.0.0.2:6443 --token-file /root/worker-join.token --plan
```

Review, then remove `--plan` to join. The private join token is copied to the managed `0600` token file before the service starts. Keep that managed credential for restarts; remove the transfer copy when no longer needed. The manager keeps its agent-only join secret for future nodes. Rotate it as coordinated K3s maintenance if compromised; Nakama's short-lived API credentials are separate.

For a later rerun after deleting transfer files, preserve all node identity/network arguments and use `--token-file /etc/rancher/k3s/nakama-agones-agent.token`. If private registry support was enabled initially, retain both registry options but point them at `/etc/rancher/k3s/registries.yaml` and `/etc/rancher/k3s/private-registry-ca.crt`. Changing these **input paths** does not change the installed profile. Omitting the registry options or changing the actual credentials/config does. Token export deliberately refuses to overwrite an existing output file; reuse the private transfer file or choose a new export path.

On the control node, verify `Ready`, node addresses and placement:

```sh
k3s kubectl --kubeconfig /etc/rancher/k3s/k3s.yaml \
  --server https://10.0.0.2:6443 get nodes -o wide --show-labels
```

For the first cluster setup, install Agones using [deployment.md](deployment.md). An additional worker joins the existing installation; no Helm reinstall or Nakama restart is required. The production values require the control labels installed here. Test a real UDP session through a GameServer on every added worker; ICMP and a `Ready` Node do not establish public game connectivity.

## Placement and resource reservations

| Role | Labels | Taint | Reserved CPU / RAM |
| --- | --- | --- | --- |
| Control | `role=control`, `game-node=false`, `agones-system=true` | `CriticalAddonsOnly=true:NoExecute` | 500m / 1152 MiB |
| Worker | `role=game`, `game-node=true`, `agones-system=false` | None | 300m / 512 MiB |

The first two labels have prefix `nakama-agones.io/`; the last is `agones.dev/agones-system`. Agones controller/extensions have hard control-node selectors and the matching toleration. GameServer pods must use `AGONES_NODE_SELECTOR_JSON` selecting `nakama-agones.io/game-node=true`. The current provider does not inject tolerations, so adding a worker taint would prevent placement.

Reservations cover the OS/K3s; Agones and game pod requests consume the remaining allocatable resources. The worker has a further 100 MiB memory eviction floor. Per-container logs rotate at 10 MiB, keeping three files; K3s's own log uses the host logrotate schedule (20 MiB threshold, five archives). That threshold is checked when logrotate runs, not a hard byte limit. Keep its timer enabled and monitor disk use. See [Kubernetes resource reservation](https://kubernetes.io/docs/tasks/administer-cluster/reserve-compute-resources/) before changing these initial budgets.

K3s registration labels/taints are not reconciled by rerunning the install script. Deliberate later label/taint changes require `kubectl`; the [agent reference](https://docs.k3s.io/cli/agent) documents this registration behavior. Do not remove a node or uninstall K3s until its game processes have drained rooms, reconnect seats and pending results.

## What adding a node changes

No Nakama restart is needed when a new worker becomes `Ready`, has matching labels/architecture, and has enough allocatable resources and free game ports. Kubernetes can schedule **still-pending** GameServers there; subsequent demand can create more processes through the same API endpoint.

The runtime's `AGONES_FLEET_MAX_INSTANCES` is a startup-loaded **process budget for this pool**, currently 1–1000. Choose an appropriate future limit at initial deployment. Adding a VPS does not increase this budget, `MAX_ROOMS`, or per-process resource requests. Changing the instance budget requires restarting the Nakama runtime with new configuration; it does not require a new game build. A new machine alone also does not create a process if there is no queued demand or minimum-instance requirement.

Pending processes have `AGONES_FLEET_LAUNCH_TIMEOUT`; allocations have `AGONES_FLEET_ALLOCATION_TIMEOUT`. A node joined before those deadlines can satisfy existing demand. Expired processes are cleaned up, and new demand can retry on newly available capacity; expired player allocations do not revive. Provision nodes before saturation, or set realistic timeouts for image pulling and startup. This version has no cloud-node autoscaler and does not buy or release VPSs.

Check `creation_blocked_reason` in the authenticated admin status before attributing a failure to node capacity. Configuration/permission/token failures during creation can persist a creation block. After fixing the cause and verifying scoped API access, use the authenticated `POST /agones/fleet/v1/admin/retry-creation` operation described in [the protocol](protocol-v1.md). A new node, successful token refresh or Nakama restart does not itself clear that stored block.
