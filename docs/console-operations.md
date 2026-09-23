# Operating Fleet Console

Install using [console deployment](console.md), then connect with the [SSH access guide](console-access.md). UI and the [versioned read API](console-api.md) observe the same sources.

## Views

| View | Use |
| --- | --- |
| Overview | Active rooms/players, available capacity, startup blocks and unhealthy sources |
| Rooms | Filter active or historical rooms; inspect Nakama user IDs, seats, reconnect state and the associated game Pod or retained scheduling record |
| Pods / workloads | All Pods in the configured observation scope; filter game Pods, region, namespace, node and phase. Pod details separate container resources from game-process diagnostics and business rooms |
| Nodes / VPS | VPS addresses, scheduling/readiness and node CPU/memory; drill down to its Pods |
| Logs | Owned game Pod/container logs, time range and live or archived source; visible system Pods do not gain log access |

Nodes represent VPS hosts. Pods are container groups scheduled on those hosts; game servers are a subset of those Pods, not a parallel resource total. A game Pod can contain the Unity game container and Agones sidecar. Rooms belong to the game process. The Pod list reflects only the authorized namespaces and server-side ownership filters, not the whole Kubernetes cluster.

Fleet records without a Pod in the latest visible inventory stay in a separate collapsible scheduling/history section and are excluded from Pod counts. A missing Pod observation does not prove it was never created or that its cloud host was removed. A source error retains clearly marked prior data. Historical game records still provide archive-log navigation. Container names may include init containers; the current API does not expose individual container state/type/resource usage, so the UI does not infer those values.

Unavailable metrics are shown as unknown, not zero. A finished room's last seat snapshot is historical state, not proof that its players remain online. Per-room CPU/RAM and player nicknames are not currently supplied. Room history is limited by Fleet state retention; the seven-day setting applies to collected logs.

## Refresh without interrupting work

Each view provides its own refresh-frequency selector, including manual-only. Automatic updates patch data in the current view rather than reloading the page or reconstructing form controls. Filters, log queries, selections, details and scroll position remain in place. Live logs have a separate follow/latest control; historical queries are manual by default.

Changing a refresh interval changes observation frequency only. It does not change the game's tick rate, heartbeat period or capacity. Snapshot collection may still have its own small cache/refresh delay.

## Graceful instance drain

Select a game Pod, review its Unity-process occupancy and confirm drain in the business section. Drain stops new room allocation to that process. Existing matches, reconnect reservations and pending durable results must finish before normal process removal. A room is an allocation inside a shared process: an instance drain is not an immediate room kick.

The optional `fleet-console-control` service must be enabled. A missing/broken broker leaves observation usable but management disabled with a reason. Browser mutations require authentication, same Origin and CSRF. The machine read-only API token cannot call them.

Creation retry is a distinct recovery operation. First repair the image, quota, credentials or API error shown in the creation block; retry then allows the runtime to resume creation. Repeatedly pressing retry does not fix the underlying cause.

There is no general Pod delete, container exec, force-kick-room or user-data editor in this console. Use the game's authoritative business protocol for a future explicit room moderation feature, including results/reconnect/audit semantics; do not delete a shared process to approximate it.

## Logs and other consoles

- Nakama Console: Nakama accounts, storage, groups and runtime diagnostics. Its Storage/Storage Indexes views are not Kubernetes volume or game-log views.
- Fleet Console: game allocations, operational metrics and query UI/API for Loki or current container logs.
- Alloy: collects selected game and Agones logs through the Kubernetes API.
- Loki: label/time/log-content queries and seven-day local retention. This is a real centralized log service, deployed here as a small single-node installation.
- Headlamp: optional general Kubernetes UI. It is not a requirement for game operations.
- Kibana/Elasticsearch: an alternative search/visualization stack, not installed here. Kafka is a streaming transport, not a log-search UI; it is not required by this stack.

The current deployment is not an HA observability platform: there is no replicated log store, long-term metric history, alert routing or distributed trace backend. Add those in response to measured operational needs. See [log storage, disk limits and failure modes](console-logs.md).

### 容器资源指标

Pod 详情按容器名称关联 Kubernetes Metrics API，分别显示应用容器与初始化容器的 CPU、内存、采样时间与窗口。资源申请和上限来自 Pod 规格，不代表实时用量。未取得采样显示未知；初始化容器可能已经退出。内存为 Metrics API 的工作集口径，不能直接等同于 Unity 托管堆或进程心跳内存。

只读 API 的 Pod 数据新增 `container_details`，保留原有 `containers` 名称数组。每项含 `name`、`kind`、`requests`、`limits`，以及存在时的 `cpu_millicores`、`memory_bytes`、`metrics_timestamp`、`metrics_window`。缺少用量字段代表没有采样，不是零。
