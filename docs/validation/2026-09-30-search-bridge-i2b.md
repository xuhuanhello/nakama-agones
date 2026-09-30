# P4i / I2b：Nakama 持久搜索薄适配候选

2026-09-30 UTC。Nakama 候选源码提交 `ca71bfb08512e2d7e61f53e195b224326922cbec` 已通过官方 runtime smoke；Nakama CI [36668798446](https://github.com/xuhuanhello/nakama-agones/actions/runs/36668798446) 与 Secret scan 36668798503 均成功。未部署生产。Fixed 客户端 I3 与真实游戏 I4 尚未完成。

匹配前先通过认证玩家 RPC 创建 search，MatchmakerAdd 只接受本人仍 pending 的 search。Matched callback 将认证 Presence、search ID 与 Nakama ticket 一起排序并使用稳定 key 调用 GameFleet 的原子匹配接口。取消 pending search 不调用房间取消；匹配已经提交时返回原 allocation，保留房间生命周期。没有增加 Nakama 分配数据库或 Kubernetes 调度逻辑。

新增三项 `gamefleet_search_*_v1` RPC；原五项房间 RPC 继续保留。Search 冲突指向查询同一次 search，不能用 `Current=null` 判断排队已结束。HTTP 客户端校验生命周期时间、scope、bound reservation 和必填 replay 字段，拒绝嵌套重复、大小写、转义及 Unicode 字段别名。

## 构建与来源

| 项目 | 实际值 |
|---|---|
| 源码提交 | `ca71bfb08512e2d7e61f53e195b224326922cbec` |
| 运行时镜像 | `nakama-gamefleet-p4i:ca71bfb` |
| 镜像 ID / repo digest | `sha256:e401f7f3c88a929156997a46a396960fbc571987866c2bc4ff2084338e431d5d` |
| Nakama 版本 | 官方 `3.41.0`（linux/amd64） |
| OCI source label | `https://github.com/xuhuanhello/nakama-agones` |
| PostgreSQL fixture | 临时镜像 `postgres@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea` |
| 源码工作树 | `source_worktree_clean: true` |

证据中的源码 revision 与 OCI revision 标签均为上述完整提交；runtime image 的本地 ID 与 repo digest 一致。安全 JSON 快照见 [runtime evidence](2026-09-30-nakama-p4i-runtime-evidence.json)。保存前扫描未发现 Bearer 凭据或数据库密码字段/值。

已完成验证：

- 完整 Go suite 与 `go vet ./...` 通过；最终搜索冲突文案修改后的 `pkg/gamefleet` / `pkg/fleetmanager` race 回归通过。
- 新增 HTTP 与 RPC 测试覆盖提交后丢响应、同身份显式重试、认证用户隔离、canonical search/ticket pair、bound cancel 保留原房间、终态历史及非法输入不调用上游。
- 跨仓库 overlay 用平台候选 `8dc25f0` 的真实 BusinessHandler、隔离 SQLite 与签名房间回执验证 begin/status/cancel/match、取消先提交拒绝迟到 match、匹配先提交保留席位，以及原有 current/assignment/consume/resume/revoke；race 运行通过（9.600 秒）。
- runtime Python 语法、内嵌 Go fixture 编译、workflow YAML 与 `git diff --check` 通过。

## 官方 runtime smoke：12 项通过

官方 Nakama 3.41.0 在临时 PostgreSQL（tmpfs、数据库端口未发布）中应用迁移，加载 `agones.so`、执行 GameFleet scope preflight 并注册 bridge。三台 synthetic device 中两台以 WebSocket 完成玩家流程。以下 12 项均通过：

1. 从零构建 local-only synthetic Business fixture 镜像，不拉取 base image。
2. 临时 PostgreSQL 就绪，数据位于 tmpfs，端口不发布。
3. 官方 Nakama 3.41.0 对隔离数据库完成自身 migrations。
4. Nakama 进程加载 `agones.so`、执行 scope preflight 并注册 GameFleet bridge。
5. 无旧 FleetManager 注册日志；无效旧 Agones DB URL 被 pilot 分支绕过。
6. 两名 synthetic 玩家通过认证，`gamefleet_current_v1` 各返回 `current: null`。
7. Search begin 创建各自 pending search；相同 request replay 返回原 search ID。
8. 两个真实 WebSocket MatchmakerAdd 通过 owned-pending 检查，matched hook 绑定精确 pair。
9. bound search status/cancel 后，单个 synthetic reserved room 仍可由 Current 读取。
10. 跨 participant status/cancel 被拒绝；已取消 search 的迟到 MatchmakerAdd 被拒绝。
11. Business scope 返回 HTTP 403 时，Nakama 在 readiness 前退出，退出码 1。
12. 无效 Business scope fail closed，不回退到 Agones。

fixture 计数：caller scope 1 次、match 前 current-null 2 次、begin（含 replay）4 次、search status 5 次、search cancel 2 次、match commit 1 次、foreign search denial 2 次。镜像、来源和其余运行事实以 JSON evidence 为准。

此 smoke 使用 synthetic Business fixture 与临时 PostgreSQL；没有验证真实 GameFleet reservation、host、endpoint、ticket 或 allocation，也没有 Fixed 实际游戏对局。P4h 的 schema 26 双周期真实进程 SIGKILL/rematch 是此前独立的 Fixed 故障验收，不能与本次 I2b runtime smoke 混为同一结果。I3 客户端与 I4 Fixed 游戏实机仍未完成，生产平台/Nakama 未改。

Nakama 远端 CI `36668798446` 与 Secret scan `36668798503` 均通过。CI 使用源码提交 `ca71bfb` 对应的官方 runtime 派生镜像，并运行上述 runtime smoke。

下一步推进 I3 结构化搜索缓存、真实 Unity Editor 编译与 I4 两个实际客户端的取消竞争验收。正式平台仍 schema 21，候选平台 schema 27 未部署，生产 Nakama 与玩家数据库均未改变。
