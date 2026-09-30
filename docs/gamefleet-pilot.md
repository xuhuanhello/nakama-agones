# Nakama → GameFleet pilot 接入说明

> 候选代码说明：本阶段未部署 GameFleet，也没有真实 Fixed 联机验收。只在明确切换后启用 pilot。业务 key 是机器凭据，不能发给客户端。
>
> 本文描述的是 Nakama 候选适配器与对应 GameFleet 候选 Business API/client 的协调契约；客户端 I3 接入和实机 I4 验收尚未完成，代码未上线，也不表示最终生产迁移或 GameFleet Business API 的生产 HTTPS gate 已完成。普通 Agones backend 未改变。

## 模式与边界

| `NAKAMA_FLEET_BACKEND` | 行为 |
|---|---|
| 未设置或 `agones` | 默认旧 Agones/FleetManager 流程 |
| `gamefleet` | 注册 GameFleet 玩家桥接；不连接旧 fleet DB，不创建 FleetManager、Kubernetes client、迁移、管理 HTTP 路由或 reconciliation worker |

旧模式仍保留。GameFleet 模式注册八个 `gamefleet_*_v1` RPC：三项 search RPC，以及 current、historical status、assignment、resume 和 reservation cancel；另注册 MatchmakerAdd admission 与 matched callback。不提供旧 `agones_fleet_*` RPC。启动会先校验 loopback Business API 和 caller scope；配置或权限校验失败时启动失败，不自动回退旧模式，也不双写分配。旧 `agones` backend 的注册与行为保持不变。

本 pilot 是单 caller scope。GameFleet owner 配置中的 participant allowlist 必须包含此 Nakama pilot 的**精确 Nakama user ID**。当前实现直接把已认证的 Nakama user ID 作为 `participantId`，没有自定义身份映射。

## 配置

复制 [gamefleet-pilot.env.example](../deploy/gamefleet-pilot.env.example) 到 Nakama 服务的环境配置，并挂载 business key 文件。除非明确准备启用 pilot，否则保持 `NAKAMA_FLEET_BACKEND` 未设置或设为 `agones`。

| 环境变量 | 用途 |
|---|---|
| `NAKAMA_FLEET_BACKEND` | 设为 `gamefleet` 启用；缺省等同 `agones` |
| `GAMEFLEET_BUSINESS_URL` | GameFleet Business API 的 loopback HTTP origin，必须是显式 IP 与端口，例如 `http://127.0.0.1:17682` |
| `GAMEFLEET_BUSINESS_KEY_FILE` | 挂载 key 文件的容器内路径；文件必须是普通文件，权限设为 `0600` 或 `0400` |
| `GAMEFLEET_APPLICATION_ID` | 预期 application ID |
| `GAMEFLEET_PLACEMENT_ID` | 预期 placement ID |
| `GAMEFLEET_REVISION_ID` | 预期 revision ID |
| `GAMEFLEET_REGION` | 精确 region 字符串，最长 128 字节 |
| `GAMEFLEET_COMPATIBILITY` | Matchmaker `build_hash` 的精确值；非空、最长 128 字节，不得有首尾空格或控制字符 |

三个 ID 只允许 1–128 位 ASCII 字母、数字、下划线和连字符。key 文件内容须为 16–4096 个非空格可打印 ASCII 字符，不要在环境变量或日志里放 key 明文。

Business URL 只接受 `http://<loopback-IP>:<port>`，不接受域名、路径、query 或 HTTPS。Nakama 容器里的 `127.0.0.1` 指向该容器自己的网络 namespace。SSH local forward 必须在 Nakama 容器内可达的 namespace/地址上监听；不要假定宿主机的 `127.0.0.1` 就是容器的 loopback。Business listener 继续限于 loopback/受控转发，不为 pilot 新开公网管理端口。

GameFleet caller scope 必须固定到上述 application、placement、revision、region，并有 `reserve`、`read`、`cancel`、`assignment`、`resume` 五项权限。Nakama 启动时会请求 caller scope 并核对四项元数据和全部权限；检查不通过就拒绝注册 hooks。participant allowlist 由 GameFleet 每次请求校验，配置为 pilot 所需的精确 Nakama user IDs。

## 匹配和恢复

客户端必须先调用 `gamefleet_search_begin_v1` 创建持久 search，再用返回的 `search.searchId` 发起双人 Matchmaker 请求。请求需带以下四个 string properties：

```json
{
  "gamefleet_protocol": "gamefleet.player-room.v1",
  "build_hash": "<GAMEFLEET_COMPATIBILITY>",
  "region": "<GAMEFLEET_REGION>",
  "gamefleet_search_id": "<search.searchId>"
}
```

`gamefleet_protocol` 使用 `RoomVersion`（`gamefleet.player-room.v1`）；search RPC 与 Business API 使用 `SearchVersion`（`gamefleet.player-search.v1`）。兼容 build 和 region 必须与 Nakama 配置一致。请求 count 为 2。入队前 Nakama 会用认证用户身份检查该 `gamefleet_search_id` 确属本人、仍为 pending 且 profile 匹配；不会改写 Matchmaker query。最终匹配时，它核对两个认证 Presence user ID、search ID 和 Nakama ticket 均有效且互不相同，再按稳定顺序提交这一整对，由 Business API 绑定 searches 并建立 reservation。玩家身份取自 Nakama `Presence` 或 RPC 的认证 `RUNTIME_CTX_USER_ID`，不接受 payload 自报 user ID；同一 matched 回调重试会使用相同幂等身份。

刚提交的新房间占用可能暂时还没出现在 host 的签名库存上报中，紧随其后的另一组匹配因此可能收到 `409`。`MatchSearches` 只对 `409`/`429` 有界重试：最多 20 次请求、总计最多 8 秒，退避从 100 ms 倍增并封顶 500 ms；caller 更短的 deadline 优先。每次重试固定使用相同 idempotency key 和原始双人快照（participant ID、search ID、Nakama ticket），不会换玩家、生成新 search 或改写身份。`401`/`403`/`404`/`422`/`503`、其他非瞬态响应及畸形成功响应都立即返回；上游 `502` 对调用者归一为 `503` 并立即返回。Begin、search cancel、assignment 和 resume 的重试语义不变。以上是候选客户端边界，不表示当前真实验收已经通过。

匹配后客户端仍收到 Nakama 普通 matched 信号。这个信号本身不保证预约成功，也不能拿 Nakama 默认 match token 直接连接游戏服。若 begin 响应不确定，使用原 `requestId` 重试以取得相同 search；若入队或 matched 通知结果不确定，使用 `gamefleet_search_status_v1` 查询已知 `searchId`。`bound` 可恢复已建立的 reservation；`pending`、`cancelled` 或 `expired` 表示仍需按对应状态处理。取得 allocation 后，`gamefleet_current_v1` 仍用于恢复当前 held reservation。

`Current` 返回 null 不能判断 search 是否 pending、cancelled 或 bound；search 状态要查 `gamefleet_search_status_v1`。reservation 释放后 current 也返回 null；null 只表示当前 caller scope 内没有 held reservation，不是跨 caller 的空闲证明。如果 current 为 null 且 search 仍 pending，客户端应按 search 状态继续等待或取消排队；不能把 matched 信号当作已有房间，也不能自行创建 fallback 房间。需要展示已释放 reservation 的最终状态时，认证玩家可单独调用 historical status。

`gamefleet_search_cancel_v1` 用于撤回仍 pending 的排队意图；它不等于离开已分配房间。若 SearchStatus 已为 `bound`，应按房间连接/退出及其自然关闭生命周期处理；reservation 的 `gamefleet_cancel_v1` 只记录取消意图，不强制关房或释放座位。

## RPC 请求

所有 payload 都是严格 JSON 对象，未知字段和尾随 JSON 会被拒绝。RPC 使用当前 Nakama 认证用户；请求中不传 `participantId`。

| RPC | 用途 |
|---|---|
| `gamefleet_search_begin_v1` | 以当前认证用户和 `requestId` 幂等创建 search |
| `gamefleet_search_status_v1` | 按 search ID 读取 pending、bound、cancelled 或 expired 状态 |
| `gamefleet_search_cancel_v1` | 取消尚 pending 的 search 排队意图 |
| `gamefleet_current_v1` | 查找认证玩家在本 caller scope 持有的 reservation |
| `gamefleet_status_v1` | 按 allocation ID 读取认证玩家有历史座位的 reservation 状态，包括释放后的终态 |
| `gamefleet_assignment_v1` | generation 0 的首次 join 票据 |
| `gamefleet_resume_v1` | 按当前持久化 generation 申请恢复票据 |
| `gamefleet_cancel_v1` | 为该玩家当前持有的 reservation 记录取消意图 |

三个 search RPC 均使用 `SearchVersion`、配置中的 `compatibility` 和 `region`。Begin 还带 8–128 位安全字符组成的 `requestId`；相同逻辑请求重试时保持该 ID。Status 和 Cancel 带 `searchId`。响应含 `search`，Begin/Cancel 另含 `replay`；bound search 可附带 allocation/reservation 信息，不返回 join ticket 或 endpoint。Room lifecycle RPC 继续使用 `RoomVersion`。

Begin 请求示例：

```json
{
  "version": "gamefleet.player-search.v1",
  "compatibility": "<GAMEFLEET_COMPATIBILITY>",
  "region": "<GAMEFLEET_REGION>",
  "requestId": "search_01JABCDEF"
}
```

Search Status 和 Cancel 的 body 将 `requestId` 换为 `searchId`，其余 profile 字段相同。

Current 请求：

```json
{
  "version": "gamefleet.player-room.v1",
  "compatibility": "<GAMEFLEET_COMPATIBILITY>",
  "region": "<GAMEFLEET_REGION>"
}
```

响应直接返回 `{"current": null}`，或类似：

```json
{
  "current": {
    "reservation": {
      "reservationId": "<id>",
      "allocationId": "<id>",
      "roomId": "<id>",
      "applicationId": "<id>",
      "placementId": "<id>",
      "revisionId": "<id>",
      "region": "<region>",
      "state": "prepared",
      "cancellationRequested": false,
      "createdAt": "<RFC3339>",
      "updatedAt": "<RFC3339>"
    },
    "connectionGeneration": 0
  }
}
```

这是恢复快照，不是在线状态证明。`connectionGeneration: 0` 表示尚无已消费连接代次，可以尝试 assignment；正数是 resume 的期望旧代次，**不表示 socket 在线**。后续票据请求仍由 GameFleet 校验 reservation 和 generation CAS。

Historical status 请求沿用 profile，并增加 allocation ID；RPC 从 Nakama 认证上下文取得玩家身份，payload 不接受 `participantId`：

```json
{
  "version": "gamefleet.player-room.v1",
  "compatibility": "<GAMEFLEET_COMPATIBILITY>",
  "region": "<GAMEFLEET_REGION>",
  "allocationId": "<allocation-id-from-current-or-prior-session>"
}
```

`gamefleet_status_v1` 不先查 current，因此 reservation 释放后仍可查询。GameFleet 按历史 seat 记录确认该认证玩家属于 reservation，并核对原 caller、application、identity issuer、placement 和 revision scope；其他 caller 或没有该 reservation 历史 seat 的玩家不能据此获知房间信息。成功响应只有 `{"reservation": ...}`，没有 replay、ticket 或 endpoint。普通 reservation 状态为 `reserved`、`prepared`、`completed`；受信任的 host 进程终止会返回 `state: "technical_aborted"` 和 `failureCode: "host_process_terminated"`。该 `failureCode` 只用于 `technical_aborted`；适配器严格校验这一组合。`Current` 不会把 completed 或 technical-aborted 的释放 reservation 当作 held reservation 返回。

Assignment 示例：

```json
{
  "version": "gamefleet.player-room.v1",
  "compatibility": "<GAMEFLEET_COMPATIBILITY>",
  "region": "<GAMEFLEET_REGION>",
  "allocationId": "<allocation-id-from-current>",
  "requestId": "join_01JABCDEF",
  "previousConnectionGeneration": 0
}
```

Resume 使用相同字段，但 `previousConnectionGeneration` 必须为 current 返回的正数，例如 `4`；`requestId` 使用新的逻辑请求 ID。`requestId` 长度为 8–128 位，只能含字母、数字、`_`、`-`。对同一 assignment/resume 请求做超时重试时，原样重发相同 `requestId`、allocation、generation 和其他字段；这会得到同一幂等操作。新的一次逻辑尝试用新 `requestId`。发生 generation 冲突时重新读 current，不能把旧请求的 generation 改掉后继续复用旧 ID。

可用票据响应包含 `assignment.ticket` 与 `endpoint`；票据是短期 bearer secret，不能写日志或通用缓存。`state` 为 `expired` 或 `superseded` 时没有可用 token/endpoint，按当前状态重新恢复。新 GameFleet 票据与 endpoint 必须交给匹配的新版连接流程；旧客户端不能把新票据当成 Agones 旧协议票据使用，也不能混合调用新旧 allocation RPC。

即使是精确重试，GameFleet 仍会复验当前 caller、房间、host 与 endpoint 观察。观察过期或 host 状态不满足条件时可返回 `409`，而不是返回过期/已替代票据状态。此时 current 仍可找到 held 房间；如果连接代次未变，保留原请求 ID 和 previous 值并退避重试，不新建房间、不连续生成新票据。

Cancel 示例：

```json
{
  "version": "gamefleet.player-room.v1",
  "compatibility": "<GAMEFLEET_COMPATIBILITY>",
  "region": "<GAMEFLEET_REGION>",
  "allocationId": "<allocation-id-from-current>"
}
```

桥接会先以认证 user ID 查询 current，并要求 allocation ID 精确匹配后才请求 GameFleet cancel；缺少 current 或 ID 不匹配时拒绝。GameFleet 的 cancel 对 held reservation 只记录持久化意图，不会强制关闭房间或释放座位；terminal reservation 的 cancel 是 no-op。只要 reservation 仍 held，后续 current 仍可返回它，并通过 `cancellationRequested` 显示取消意图。当前没有接入游戏房间的取消 callback，因此不能把 cancel 成功描述成玩家已退出或房间已结束。有效 natural-close 生命周期仍负责释放 reservation；释放后 current 为 null，status 用于读取历史终态。

## 上线前检查

- [ ] Nakama 容器内能连接配置的 loopback Business URL；未暴露公网管理口。
- [ ] Business key 文件在容器中为普通文件，权限 `0600` 或 `0400`；不要把 key 打入镜像或日志。
- [ ] GameFleet caller scope 元数据一致，含全部五项权限；participant allowlist 是精确 Nakama user ID。
- [ ] 客户端请求的 protocol/build/region 与服务端配置完全一致；pilot 新旧客户端与 RPC 不混用。
- [ ] 用隔离测试凭据验证 current 恢复、释放后的双人 status 读取、同一 `requestId` 重试、resume generation 冲突、cancel 仅记录意图。
- [ ] 记录 pilot 仍未部署、未进行真实 Fixed 联机验收；通过本清单不等于生产启用。

本说明依据 `pkg/gamefleet/{setup,bridge,client,model}.go` 和 `pkg/fleetmanager/setup.go` 当前实现编写。

## 本阶段验证

I2b 官方运行时 smoke 已通过：提交 `ca71bfb` 的 [Nakama CI 36668798446](https://github.com/xuhuanhello/nakama-agones/actions/runs/36668798446) 成功，Secret scan 36668798503 也通过。官方 Nakama 3.41.0 加载候选插件，合成玩家通过 WebSocket 执行 search、MatchmakerAdd/Matched、cancel 与越权拒绝；启动 scope 拒绝也按预期失败关闭。详见 [I2b 验收记录](validation/2026-09-30-search-bridge-i2b.md)。它使用临时 PostgreSQL 和 synthetic Business fixture，不是真实 GameFleet allocation、host/ticket/endpoint 或 Fixed 对局；客户端 I3 和真实 Fixed 游戏 I4 仍未完成，生产未部署。

本候选已通过新旧模式、全部 Go 单元测试、`go vet` 与 race 检查。新增测试覆盖同匹配回调反序重放、认证身份、他人房间取消、同次入场的稳定请求 ID、未授权/缺字段响应、代理/重定向隔离及启动时 scope 校验。协调后的 P4h 隔离 HTTP 验证还确认同一 reservation 的两名历史 seat 玩家都能读取 status，技术终止后 current 为 null 而 status 保留 terminal 状态；这仍是候选隔离验收，不是最终生产迁移或 HTTPS Business API gate 验收。

P4h 的 schema 26 双周期 SIGKILL/rematch 是前一阶段的 Fixed 故障验收；本节 I2b 则是官方 Nakama 插件与 search bridge smoke。两项使用的系统边界不同：P4h 不证明 I2b 插件 runtime，I2b 的 synthetic fixture 也不证明真实 Fixed 对局。

另外用 GameFleet 候选提交 `3bed77d8fdc8bd68bee71f0c728fdfb3f840bc87` 的真实 `BusinessHandler`、隔离 SQLite、自动生成测试 Key 和签名房间回执完成了跨仓库 HTTP 合约验证：current、reserve replay、join replay、consume 后 superseded、resume、stale endpoint、cancel held 与 key revocation。复现：

```sh
./scripts/test-gamefleet-contract.sh /path/to/selfhosted-gamefleet-candidate
```

此脚本要求本机能运行 Go 1.27.1，并可能下载该工具链/模块；只使用临时 modfile 和源码 overlay，不修改任一仓库的 go.mod，不使用部署配置或真实凭据，不登录 VPS。测试 fixture 位于 `tests/contracts/gamefleet_business_test.go.txt`，依赖该 GameFleet 候选中的测试辅助函数。

此前 P4d 的官方 Nakama 加载与认证 RPC 验收见[记录](gamefleet-runtime-validation.md)；本次 I2b 新增 search/matchmaker runtime 检查，证据见上方记录。Fixed Linux 镜像、真实平台 allocation 和 FishNet 双人对局仍待隔离试点验证。
