# GameFleet 桥接：官方 Nakama 进程验收

2026-09-29，候选插件源码提交
`924ddf85b94b5eade91e57c9d4ec1991eb5325c2` 已完成真实官方运行时加载验收。
本记录只覆盖插件 ABI、启动权限检查和已认证用户 RPC，不代替真实 GameFleet 分配或 Fixed 双人对局。

## 构建身份

| 项目 | 验证值 |
| --- | --- |
| Nakama | `3.41.0+cab8af5` |
| Go / nakama-common | `1.27.1` / `1.48.0` |
| 官方 runtime digest | `sha256:ca9f3fe65c90f56860fe5d7cd024868a395cb1a001abb9912bf87ef47d605349` |
| 官方 pluginbuilder digest | `sha256:eb506121ec2e67f39febee3a4252ae05598a0b39d3517b713ec1577714a9e5b5` |
| 本地候选 image ID | `sha256:918d5111b749c93b9c40d33b28a20f8cb8dac8fe6b89311f190cf6539fa24999` |
| `agones.so` SHA-256 | `8605d3deeb6588eee8e516587643f99c2efd13bf019da6cd887759a8d7747947` |

源码通过该提交的 `git archive` 导出到干净临时构建上下文，然后运行 `deploy/Dockerfile`。
没有将本机私有配置加入上下文。官方 builder 的编译、兼容性检查、官方 runtime 真实进程均通过。
上述本地验收镜像使用默认 `VCS_REF=uncommitted` 标签；源码身份以干净导出的提交和插件哈希为据。
后续重建应像下文一样显式设置 `VCS_REF`。镜像没有推送或部署。

## 已观察结果

- 临时 PostgreSQL 16 的数据目录仅在 tmpfs；5432 不映射到宿主机。
- Nakama 与最小 Business fixture 使用同一容器网络 namespace，Business 只监听 `127.0.0.1:17682`。
- Nakama 7350 仅映射宿主机 `127.0.0.1` 随机端口；不读取任何线上凭据或数据库地址。
- 官方 Nakama 迁移只作用于本次临时数据库。Nakama 加载真实 `agones.so`，记录 GameFleet 注册日志，`/healthcheck` 成功。
- 故意设置无效的旧 Agones DB URL 后仍可启动；没有旧 FleetManager 注册日志。
- 两名临时设备用户分别通过 Nakama 认证，并由 `gamefleet_current_v1` 调用 loopback Business fixture，返回 `current: null`。
- fixture 返回 HTTP 403 时，Nakama 在注册完成前退出，实际退出码 **1**；没有回退到旧后台。
- 验收结束清除本次容器、网络和 fixture 镜像。保留候选 runtime 镜像用于后续本地验证。

fixture 只实现 `/caller` 和空的 `/reservations/current`。它没有真实 reservation、host、签名票据或
游戏连接；这部分真实 API/账本行为由独立 BusinessHandler 合约测试覆盖，实机对局仍是下一步。

## 复现

需要本机 Docker、Python 3、Go，以及能运行 Linux/amd64 容器的环境。
先拉取 Dockerfile 中固定的官方 runtime 与 builder，另准备 `postgres:16-alpine`；测试脚本使用
`--pull=never`，不会临时拉取不明确的镜像。它不会操作现有容器或现有数据库。

在干净候选 checkout 根目录：

```sh
revision=$(git rev-parse HEAD)
docker build --platform linux/amd64 --file deploy/Dockerfile --target runtime \
  --build-arg VCS_REF="$revision" --tag nakama-gamefleet:runtime-check .
python3 scripts/test-gamefleet-runtime.py --repo . \
  --image nakama-gamefleet:runtime-check --evidence /tmp/gamefleet-runtime-check.json
```

测试创建自己唯一 `gf-p4d-*` 名称的临时资源，生成临时数据库密码，使用显式合成 Business key，
在结束/失败时清理本次资源。`--temp-root` 可以指定临时目录父路径；容器 bind mount 要能访问该路径。
`--positive-only` 仅用于排查加载阶段，不能作为完整通过证据。JSON 报告区分完整通过与仅正向通过。

报告中的 checkout revision/clean 状态是运行测试时的源码环境，不能单独证明任意 `--image` 的来源；
部署时仍须保留构建提交、image digest 和插件哈希对应关系。

现有 GitHub `Validate Agones integration` 仍含旧 k3d 流程。此前 registry 429 导致它失败，
本次本地成功不将该历史 CI 改写为通过，也不意味着完成了新平台上的游戏 Pod 验收。
