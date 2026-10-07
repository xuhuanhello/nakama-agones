# Nakama GameFleet service adapter

本仓库为 Nakama 提供唯一受支持的 GameFleet 接入方式：`gamefleet-service`。GameFleet 保存唯一分配账本；Nakama 负责玩家身份、匹配与恢复 RPC；游戏 Host 执行房间生命周期。玩家数据库、登录插件和应用配置由游戏应用提供完整 Nakama 镜像。

从空 Linux 主机开始安装，请按唯一的 [部署手册](docs/deployment.md) 操作。该手册创建 PostgreSQL、Nakama 和 HTTPS 网关的完整 Compose 栈。`scripts/nakama_stack.py init` 只生成模板及本地私钥；`validate` 检查配置、权限、镜像身份和 Compose 模型；它不会部署或探测远端健康状态。新装目录和 TLS 凭据由 root 管理，因此初始化、校验和 Compose 部署命令均通过 `sudo` 执行。

核心参考：

- [架构与端口边界](docs/architecture.md)
- [service 身份、授权与 RPC 协议](docs/gamefleet-service-runtime.md)
- [Nakama / Go 插件兼容组合](docs/compatibility.md)
- [单机新装部署](docs/deployment.md)

Fixed 的完整应用镜像必须包含本仓库构建的 `/nakama/data/modules/agones.so`，并在使用账号/邮箱功能时包含 Fixed 提供的 `account.so`。Compose 接受本机 Docker image ID 或拉取后的完整 OCI digest，并禁止 Compose 静默拉取应用镜像。

Nakama 到 GameFleet 使用独立业务 HTTPS 入口和双向证书认证，优先已验证的私网路由。CA、客户端证书与私钥通过只读文件注入；`gfsvc_` 凭据继续独立验证业务授权，不依赖 SSH sidecar。

本地或 CI 测试通过不等于平台授权、DNS/TLS、账号登录、匹配分配或生产健康验收完成。请按 [部署手册](docs/deployment.md)逐项验证真实运行状态。

[MIT](LICENSE)，来源见 [UPSTREAM](UPSTREAM.md)。
