# P4i / I2b：Nakama 持久搜索薄适配候选

2026-09-30 UTC。未部署生产；Fixed 客户端 I3 与真实游戏 I4 尚未完成。

匹配前先通过认证玩家 RPC 创建 search，MatchmakerAdd 只接受本人仍 pending 的 search。Matched callback 将认证 Presence、search ID 与 Nakama ticket 一起排序并使用稳定 key 调用 GameFleet 的原子匹配接口。取消 pending search 不调用房间取消；匹配已经提交时返回原 allocation，保留房间生命周期。没有增加 Nakama 分配数据库或 Kubernetes 调度逻辑。

新增三项 `gamefleet_search_*_v1` RPC；原五项房间 RPC 继续保留。Search 冲突指向查询同一次 search，不能用 `Current=null` 判断排队已结束。HTTP 客户端校验生命周期时间、scope、bound reservation 和必填 replay 字段，拒绝嵌套重复、大小写、转义及 Unicode 字段别名。

已完成验证：

- 完整 Go suite 与 `go vet ./...` 通过；最终搜索冲突文案修改后的 `pkg/gamefleet` / `pkg/fleetmanager` race 回归通过。
- 新增 HTTP 与 RPC 测试覆盖提交后丢响应、同身份显式重试、认证用户隔离、canonical search/ticket pair、bound cancel 保留原房间、终态历史及非法输入不调用上游。
- 跨仓库 overlay 用平台候选 `8dc25f0` 的真实 BusinessHandler、隔离 SQLite 与签名房间回执验证 begin/status/cancel/match、取消先提交拒绝迟到 match、匹配先提交保留席位，以及原有 current/assignment/consume/resume/revoke；race 运行通过（9.600 秒）。
- runtime Python 语法、内嵌 Go fixture 编译、workflow YAML 与 `git diff --check` 通过。

官方 Nakama 3.41.0 容器加载、实际 WebSocket 入队/匹配和权限失败关闭仍在验收。该运行时脚本使用临时 PostgreSQL 和 synthetic Business fixture，验证插件运行，不等于真实 GameFleet 分配或 Fixed 对局；真实平台 HTTP 契约由上面的 overlay 单独验证。CI 增加运行时步骤，复用旧集成已经构建的官方版本派生镜像。

后续先核定运行时及 CI，再推进 I3 结构化搜索缓存、真实 Unity Editor 编译与 I4 两个实际客户端的取消竞争验收。正式平台仍 schema 21，候选平台 schema 27 未部署，生产 Nakama 与玩家数据库均未改变。
