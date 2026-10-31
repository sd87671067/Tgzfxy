# sbbot Go

用于 Debian / Docker 的 sing-box Telegram 管理机器人。本仓库更新为本机 `sbbot` 容器所用的 Go 项目源码；旧 Python 版本可从 Git 历史查阅。

支持入站/出站及分享链接、路由绑定、流量统计、月额度、有效期、UFW、搬瓦工管理，以及 Telegram 频道转发、用户账号登录、关键词筛选、按钮链接展开和失败重试。入口为 `cmd/sbbot`。

## Docker 部署

```bash
cp .env.example .env
chmod 600 .env
# 编辑 .env，填写 TELEGRAM_BOT_TOKEN 和 TELEGRAM_ADMIN_IDS
# 转发功能另需 FORWARD_API_ID 和 FORWARD_API_HASH
# 按需设置域名、证书、sing-box API 和搬瓦工配置
mkdir -p data
chmod 700 data
docker compose up -d --build
```

部署模板使用 host 网络、host PID 和 privileged，以便通过 `nsenter` 管理宿主机 sing-box 与 UFW。仅在自己的受信任服务器上部署。sing-box 默认位于宿主机 `/usr/local/bin/sing-box`，配置目录默认 `/etc/sing-box`，可通过 `.env` 设置挂载目录。证书目录通过 `CERTS_DIR` 以只读方式挂载；`ANYTLS_CERT_PATH`、`ANYTLS_KEY_PATH` 填写挂载后的绝对路径。证书续期由宿主机维护。

管理员 ID 必须为正整数，多个值用逗号分隔。可选功能的凭据留空即可。首次启动会按配置初始化 Clash API；是否自动配置和重启 sing-box 分别由 `AUTO_CONFIGURE_CLASH_API`、`AUTO_RESTART_SING_BOX` 控制。

## 数据与迁移

数据库、Telegram 登录会话及运行文件保存在 `data/`。从 Python 迁移前，先停止旧实例，备份 Compose、环境配置和数据库，再将原有数据目录接入新部署。程序支持读取原 SQLite 表结构，并在需要时转换已有 Telethon 会话。不要同时运行使用同一 Telegram 用户会话的两个实例。

规则和转发进度持久化；失败重试队列保存在内存中，最多 500 项、24 小时过期，重启后清空。重试发送从失败回复提取的缓存链接。不要将旧 Python 文档关于持久重试队列的描述套用到此版本。

## 源码构建

项目使用 Go 1.24.4 工具链和 CGO（SQLite），还需要 C 编译器。Dockerfile 包含完整的多阶段源码构建。MTProto 依赖首次编译较耗内存和时间。

```bash
GOMAXPROCS=1 GOMEMLIMIT=700MiB go build -p 1 -gcflags=github.com/gotd/td/tg=-l -ldflags="-s -w" -o bin/sbbot ./cmd/sbbot
```

运行时默认 `GOMAXPROCS=2`、`GOMEMLIMIT=64MiB`，后者是 Go 软内存目标。

本仓库包含单元测试源码；按仓库约定，正式测试仅使用 `sd87671067/go-emby-test`，测试前必须核实目标仓库，不在本公共仓库执行正式测试。

## 维护

```bash
docker compose exec sbbot /usr/local/bin/sbbot -check
docker compose exec sbbot /usr/local/bin/sbbot -maintenance
```

`-check` 只读检查配置和已有数据库，不联网；`-maintenance` 清理过期操作日志。`-probe` 会联网验证 Telegram 认证；使用已有用户会话前应先停止原实例。

公开内容只包含源码、合成测试样例及空白环境模板。真实 `.env`、数据库、登录会话、配置备份、证书、私钥和预编译二进制均不提交。

## 统一 VLESS REALITY 用户管理（显式启用）

新功能不会自动合并旧入站，也不会接管 TCP 443。先用 `cmd/sbbot-plan`
生成私有候选文件，离线审阅 UUID、出口、历史额度、到期和客户端链接后再安排部署。
工具默认使用回环地址及 18443；仅检查配置内端口重复，不证明宿主机端口可绑定。
候选入口 tag 使用 `sbbot-unified-` 前缀，复用指定模板的 REALITY 参数。
私钥、UUID 提案、出站密码和候选配置均不得提交到 Git。

```bash
go run ./cmd/sbbot-plan -config /path/to/source.json \
  -template legacy-tag -output /private/candidate.json \
  -mapping-output /private/migration.json
```

输出文件必须不存在，权限为 0600。候选保留所有旧监听/用户/规则，并增加默认拒绝的空统一入口。
迁移提案按原配置顺序为重复 UUID 的后续用户建议新 UUID；这只是提案，不代表批准。
旧客户端仍使用原端口、UUID 和 REALITY 参数。不同模板的公钥不能自动兼容统一入口，
所有迁移用户须审阅并导入新链接；保留旧 UUID 也不代表旧链接能直接连接新端口。
有条件路由的旧入口需人工审阅，工具拒绝猜测出口。

预置并验收统一入口后，管理员可使用：

- `/adduser 统一入口tag 名称 出站tag [UUID]`：添加独立用户；省略 UUID 时生成新值。
- `/users` 或「协议管理 → UUID 用户与出站 IP」：点击 UUID → 出站地址 → 一次性确认。
- `/share user/入口tag/名称`：单用户链接；`/share 统一入口tag` 生成全部用户链接。
- `/usage`、`/alllimit`、`/setexpiry`、`/block`、`/unblock` 接受上述用户标识。
- `/adopthistory 用户标识 旧入站tag`：先预览，确认后关联历史流量、继承额度、到期和手动暂停。

出站按钮显示配置中的上游地址，实际落地 IP 须通过独立网络探测确认。
确认按钮按管理员会话保存，限制聊天、消息、有效期和配置版本，执行前消费 token 防止重放。
统一变更必须启用 `AUTO_RESTART_SING_BOX`，并经过候选 `sing-box check`、原子替换、重载及运行状态检查。
重载会断开已有 sing-box 连接，客户端需要重连；这不涉及其他服务的重启。
失败时使用原配置恢复；恢复本身失败会返回明确错误。配置备份是私有文件，需由管理员制定保留策略。
`CONFIG_PATH` 的 `/config` 目录对应宿主机 `/etc/sing-box`；其他挂载布局需要适配预检路径。

绑定直接持久化在 sing-box JSON 中。每个用户由 `auth_user` 路由到专属、仅含一个真实出口的
selector；Clash 连接 `chains` 中保留此标识，因此多个用户共用出口仍能分别统计。
禁止整入口覆盖用户绑定、删除在用出口和删除仍有用户的统一入口，未知或未绑定用户默认拒绝。
手动暂停及超额/到期生成单用户拒绝规则。已有转发、MTProto、旧入站功能保留。

SQLite 仅新增 `user_legacy` 关联表，不重写旧 samples/limits/proto_expiry。
历史关联拒绝覆盖已有设置；关联后旧、新客户端合计计量，超额时同时暂停。
用户额度/到期及历史关联在配置应用失败时恢复相应数据库状态。
流量继续采用 Clash 轮询：完整落在两次采样之间的短连接可能漏计，崩溃或长时间 API 故障也可能丢样；
不应把它当作严格计费系统。默认额度及已有调度间隔保持原语义。

针对统一入口的网络集成测试只创建回环监听、合成用户和临时 SQLite，不发送 Telegram 消息：

```bash
SING_BOX_BINARY=/usr/local/bin/sing-box go test ./internal/sbbot
```

测试会访问 REALITY 握手目标 `www.samsung.com:443` 和 HTTPS IP 探测站，网络不可达会导致失败。
可选 `SBBOT_MAPPING_FIXTURE` 仅用于读取私有配置、验证迁移映射和候选 check，绝不安装或启动该候选。

Behind HAProxy, set `UNIFIED_PUBLIC_PORT=443` so `/share` uses the public port for unified users. Legacy listener links retain their own ports.

### 自然月额度、响应与维护

- 当月流量使用 `TIMEZONE`（默认 `Asia/Shanghai`）自然月窗口；不删除历史、不清空连接基线。启动及每次采集重新计算限制，停机跨月后也自动恢复仅因月额度暂停的用户。手动暂停、原到期时间及 `/alllimit` 周期额度/到期独立生效。
- 已移除旧月额度命令。数据库 `limits`、历史流量和其他功能共用的限额逻辑保留；使用 `/alllimit 用户标识 [MB] [天]` 设置独立周期。
- 无启用转发规则时无 MTProto 连接、无 30 秒轮询定时器、无定期规则查询。创建/启停/删除规则发出变更通知；打开登录或绑定时按需连接并给予十分钟会话租期。启用规则后保持实时事件、遗漏补取、过滤和失败重试；队列与回复关联持久化到 `retry_state`，停用不丢弃队列。修改数据库规则的外部工具须重启 Bot 使变更生效。
- Telegram polling 与管理操作分离；callback 在入队前以三秒超时应答，128 项有界队列以单 worker 按序执行，操作完成后发送/编辑结果。日志记录 `callback_ack` 和 `bot_operation` 耗时。队列仅驻内存，进程异常退出时未执行的管理操作需要重试。
- `/alllimit` 不再 `VACUUM INTO` 整库；变更前原策略以 0600 权限原子写入 `sbbot.db.policy-recovery.json`，运行中失败恢复旧策略。部署前仍须使用 SQLite backup API 创建独立完整备份。
- 启动十分钟后、随后每六小时执行限时维护：分批清除超过 24 小时的操作/转发日志和过期转发缓存、轮转日志，仅执行 PASSIVE checkpoint。不删除 samples、limits、user_cycles、sent、retry_state、会话、配置或备份，不执行 VACUUM、不直接删除 WAL/SHM。
- Docker 建议使用 local 日志驱动，`max-size: 5m`、`max-file: 2`。部署只替换 sbbot 并保留当前健康镜像及回滚版本。

### 分享地址与本机 API 密钥

`SHARE_HOST` 显式配置始终优先，支持公网地址或域名；不配置时通过有超时和响应大小限制的 HTTPS 服务探测公网 IPv4，失败时要求配置，不使用 `hostname -I` 的私网地址。命令、统一入口和菜单复用同一分享地址及链接生成器，保持 VLESS Reality 参数与公开端口行为。

`SINGBOX_API_SECRET` 应使用安全随机源生成至少 32 字节，并与 sing-box 的 `experimental.clash_api.secret` 同步更新；API 只监听 `127.0.0.1:9090`。生产环境文件、数据库、证书、私钥、配置和备份不得提交。历史敏感内容应先撤销凭据，历史改写须单独授权。
