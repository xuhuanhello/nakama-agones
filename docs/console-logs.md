# Console logs: seven-day pilot

Apply `deploy/observability/` to add one Loki and one Alloy on the existing control node. Images are pinned by version **and multi-platform digest**: [Loki 3.7.8](https://github.com/grafana/loki/releases/tag/v3.7.8) and [Alloy 1.19.2](https://github.com/grafana/alloy/releases/tag/v1.19.2), checked 2026-09-22. Loki stores logs locally; this adds no application database, Grafana service or public port.

| Control-node workload | CPU request / limit | Memory request / limit | Local PVC |
| --- | --- | --- | --- |
| Loki, single binary | 100m / 500m | 384Mi / 768Mi | 10Gi |
| Alloy, one collector | 50m / 250m | 128Mi / 256Mi | 256Mi for read positions |

The combined memory limit is 1Gi on the 2-core/4GiB control VPS. Existing K3s/Agones workloads still need headroom; monitor actual usage before increasing logging or query concurrency. Both Deployments use `Recreate`, `nakama-agones.io/role=control`, and the existing `CriticalAddonsOnly=true:NoExecute` toleration. No logging DaemonSet or host-path mount is added to 2GiB game nodes.

## Collection and access

Alloy tails Kubernetes `pods/log` for `agones-games` Pods labelled `app.kubernetes.io/managed-by=nakama-agones` (`game` and `agones-gameserver-sidecar`) and Agones controller/extensions Pods in `agones-system`. Nakama, databases and other namespaces are excluded. Its separate ServiceAccount can only get/list/watch Pods and get their logs in those two namespaces. Label filters are collector configuration; Kubernetes RBAC does **not** enforce per-label authorization within a namespace.

The [API-based collector](https://grafana.com/docs/alloy/latest/reference/components/loki/loki.source.kubernetes/) avoids node filesystem access but adds API-server, kubelet CPU and network work. It drops common token/password/key/authorization/DSN lines, recognizable JWT/private-key material and oversized lines before sending. Only `cluster`, `namespace`, `pod` and `container` become labels; room/user IDs never become indexed labels. These conservative filters are defense in depth, not a guarantee against every secret format. Game code must continue avoiding credentials in logs.

`fleet-loki:3100` is ClusterIP-only. The console's `agones-control/fleet-console` identity receives only `get` on this exact Service's proxy:

```text
/api/v1/namespaces/agones-observability/services/http:fleet-loki:http/proxy/loki/api/v1/query_range
```

The console must constrain query parameters, namespaces, time range and returned line count; do not expose an arbitrary proxy URL. Set Alloy's `FLEET_LOG_CLUSTER` to the console's corresponding `region.name` (for example `us-west`); the generic manifest defaults to `nakama-agones`, which must be changed when the console uses another name. Access the console through the existing SSH tunnel to its loopback listener; no Caddy route or firewall opening is required. NetworkPolicy permits Alloy ingress to Loki and relies on Kubernetes' [own-node traffic allowance](https://kubernetes.io/docs/concepts/services-networking/network-policies/) for the co-located control API server. Revisit that rule before adding control nodes or changing CNI behavior.

## Install and verify

Use the intended cluster's explicit administrative kubeconfig. `agones-games`, `agones-system`, the console ServiceAccount, and K3s `local-path` storage must already exist. Verify free disk and control-node capacity first. Also check `kube-system/local-path-config`'s `helperPod.yaml`: its provisioning helper needs the same `CriticalAddonsOnly` toleration on this tainted control node. Add that narrowly if absent; a toleration on the provisioner Deployment alone does not protect its helper from `NoExecute` eviction. Also ensure the provisioner has `CONFIG_MOUNT_PATH=/etc/config/`: without it, helper-template changes are not reloaded until the provisioner restarts ([upstream behavior](https://github.com/rancher/local-path-provisioner#reloading)). Setting this environment variable rolls only the storage provisioner. Preserve the existing ConfigMap and re-check these customizations after K3s system-addon upgrades.

```sh
kubectl --kubeconfig /secure/cluster.yaml apply -f deploy/observability/00-namespace.yaml
kubectl --kubeconfig /secure/cluster.yaml apply --dry-run=server -f deploy/observability/
kubectl --kubeconfig /secure/cluster.yaml apply -f deploy/observability/
kubectl --kubeconfig /secure/cluster.yaml -n agones-observability rollout status deployment/fleet-loki
kubectl --kubeconfig /secure/cluster.yaml -n agones-observability rollout status deployment/fleet-alloy
kubectl --kubeconfig /secure/cluster.yaml get --raw '/api/v1/namespaces/agones-observability/services/http:fleet-loki:http/proxy/ready'
```

Verify console identity can GET that proxy but cannot POST it, proxy another Service, or read Secrets. Confirm selected game/SDK/controller logs appear, sensitive fixtures do not, and a finished game's already ingested logs remain queryable after Pod deletion. Readiness alone does not prove collection or delivery. Changing a ConfigMap does not restart these Deployments; explicitly restart only the affected Deployment after validation.

## Retention and limits

TSDB v13 uses a 24-hour index period and [Compactor retention](https://grafana.com/docs/loki/latest/operations/storage/retention/). Logs are eligible for deletion after **168 hours from their log timestamp**, not seven additional days after a game exits. Queries cannot look back beyond seven days. Physical deletion follows compaction and a two-hour delay; it is not an exact wall-clock deletion guarantee. Chunks, indexes, WAL, delete requests and Compactor deletion markers all stay on the Loki PVC across Pod replacement.

The [filesystem store](https://grafana.com/docs/loki/latest/operations/storage/filesystem/) offers neither HA nor automatic disk-capacity retention. K3s local-path's requested 10Gi is not a hard quota. Keep at least 30% host-disk headroom; alert on disk growth, compactor errors, OOM/restarts, Loki rejected writes, and Alloy `loki_write_dropped_entries_total`. A rough initial budget is **under 1Gi/day of stored data**, leaving space for indexes/WAL; measure it rather than assuming compression. A full/lost local disk, PVC deletion or lost manager can lose logs. Do not delete PVCs during routine upgrades or manually age-delete Compactor marker files.

Persisted Alloy positions improve restart recovery but do not make delivery exactly once. This stable configuration does not enable Alloy's experimental write WAL. Outages, retry exhaustion, rotation or a Pod deleted before collection can lose un-ingested lines, and recovery can duplicate lines. Seven-day retention applies to data successfully stored by Loki; this is an operational log viewer, not the game's durable result/audit receipt store.

Local validation requires Python with PyYAML; `--images` additionally uses Docker, never a cluster or credentials:

```sh
python3 deploy/observability/validate.py --images
```
