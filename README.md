# Nakama GameFleet 适配

将 Nakama 的认证玩家、Matchmaker 配对与恢复请求接入独立 [Self-hosted GameFleet](https://github.com/xuhuanhello/selfhosted-gamefleet)。Nakama 负责身份与配对，平台负责唯一资源/房间账本，游戏进程负责玩法；不需要修改 Nakama 或官方 Unity SDK 源码。

当前 Fixed 集成使用 `gamefleet-service` 模式。仓库仍保留早期 Agones 直管和普通 caller 模式，安装时必须显式选择一种，不能同时注册多个 FleetManager / matched hook。

## 使用入口

1. [架构](docs/architecture.md)：组件与权限边界、匹配和恢复时序。
2. [service 配置与协议](docs/gamefleet-service-runtime.md)：当前接入的参数、身份、授权和 RPC。
3. [环境模板](deploy/gamefleet-service.env.example)：填写非秘密标识，密钥通过只读文件挂载。
4. [编译兼容性](docs/compatibility.md)：插件必须匹配 Nakama 运行时/编译器/依赖组合。
5. [部署说明](docs/deployment.md)：旧 Agones 直管部署仅用于该明确模式，不能用它安装新 GameFleet 平台。

快速路径：先部署 GameFleet 并配置 service 的身份/issuer、search/match/history 授权和发布路由；再按 service 文档构建与启动 Nakama 插件组合。配置 `NAKAMA_FLEET_BACKEND=gamefleet-service`，挂载独立 `gfsvc_` key 文件并检查启动 preflight，最后接入匹配客户端。数据库、邮箱等应用插件由应用仓库维护。

基础模板不是完整业务安装器；`GAMEFLEET_SERVICE_URL` 必须从 Nakama 进程网络空间可达。主机 loopback 不自动等于容器 loopback，不能通过公开管理端口来绕过转发配置。

本仓库提供本地 `init`、`validate`、`plan`、`build`、`render` 与 `status` 命令用于准备 Nakama service 配置；`render` 只生成供 Fixed 现有 Compose 项目审阅的覆盖片段，不会安装平台或部署服务。详见 [本地准备步骤](docs/gamefleet-service-runtime.md#local-self-service-preparation)。

## 协议与运行原则

认证身份来自 Nakama 上下文；玩家不能自报 user ID、caller、placement 或 revision。搜索/匹配幂等键与连接代次由协议约束；响应超时按原请求查询/重试，不创建 fallback 房间。历史查询与新匹配授权分开，发布切换不能破坏旧分配恢复。

GameFleet 是分配唯一写入者，新模式不把 Kubernetes 管理凭据、节点 SSH 或第二套占用账本交给 Nakama。完整总游戏设计由 [Fixed](https://github.com/xuhuanhello/Fixed-Point-Physics-C-Sharp)维护，平台安装由 [GameFleet](https://github.com/xuhuanhello/selfhosted-gamefleet/blob/codex/production-nakama-admission/docs/bootstrap-console.md)维护。

## 验证与许可

源码测试、隔离运行时和已部署游戏的证据必须区分。此 service 候选有运行时验证，Fixed v6 的生产链路另有游戏验收；新机器上的完整重建尚在进行，不能据此前结果宣布空机验收通过。

[MIT](LICENSE)，来源见 [UPSTREAM](UPSTREAM.md)。同一套架构和安装文档原地更新；阶段验收记录保留其版本边界，不作为新安装入口。
