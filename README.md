# Tgzfxy — sbbot

通过 Telegram 管理 Debian 宿主机上的 sing-box：入站/出站管理、订阅链接、流量统计、月额度和到期暂停；可选搬瓦工管理与 Telegram 频道转发助手。

## 运行条件

- Debian Linux、Docker Engine 和 Docker Compose 插件。
- 宿主机已安装并配置 sing-box，使用 `sing-box.service`；配置目录默认 `/etc/sing-box`，其中需有有效的 `config.json`。本项目不附带任何用户的入站、出站或证书。
- 自动重启和防火墙管理使用宿主机 systemd、UFW 和 `nsenter`。Compose 使用 host 网络、host PID 和 privileged，具有宿主机管理能力，仅授权可信管理员。
- AnyTLS / Hysteria 2 需要宿主机 sing-box 支持相应协议，域名解析正确，证书有效，并允许对应 TCP / UDP 端口。

## 安装与配置

克隆公共仓库后进入项目目录：

```bash
git clone https://github.com/sd87671067/Tgzfxy.git
cd Tgzfxy
cp .env.example .env
chmod 600 .env
nano .env
docker compose up -d --build
docker compose logs --tail=50 sbbot
```

必须填写 `TELEGRAM_BOT_TOKEN`（BotFather 创建的机器人 token）和 `TELEGRAM_ADMIN_IDS`（自己的正整数用户 ID，多人用英文逗号分隔）。未设置任意一项，或 ID 格式错误，进程退出码为 1，日志会提示在 `.env` 中设置；默认最多重试三次。填写真实有效 token 后在 Telegram 私聊机器人发送 `/start`。

机器人环境参数填写本地 `.env`，不要编辑源代码加入密钥。入站、出站及其账号密码由使用者通过协议管理自行添加，保存在本机 sing-box 配置中，不随仓库提供。环境配置变更后执行：

```bash
docker compose up -d --force-recreate
```

仅 `docker compose restart` 不会加载修改后的环境变量。

| 变量 | 用途 |
| --- | --- |
| TELEGRAM_BOT_TOKEN / TELEGRAM_ADMIN_IDS | 必填，机器人与授权管理员 |
| FORWARD_API_ID / FORWARD_API_HASH | 可选，Telegram 开发者应用凭据，从 my.telegram.org 获取 |
| BWH_API_KEY / BWH_VEID | 可选，搬瓦工服务 API 与 VPS 编号 |
| SHARE_DOMAIN | AnyTLS / Hysteria 2 的 TLS 域名，无内置个人域名 |
| SHARE_HOST | 分享链接连接地址，可填自己的域名或 IP；留空时从本机网络推断 |
| CERTS_DIR | 本地证书目录，同路径只读挂载至容器，默认 /root/certs |
| ANYTLS_CERT_PATH / ANYTLS_KEY_PATH | AnyTLS 和 Hysteria 2 共用的证书完整链及私钥绝对路径 |
| REALITY_SNI | VLESS Reality 的默认伪装域名 |
| SINGBOX_CONFIG_DIR | 宿主机 sing-box 配置目录，默认 /etc/sing-box |
| SINGBOX_API_URL / SINGBOX_API_SECRET | 本机 Clash API 地址及密钥 |
| AUTO_CONFIGURE_CLASH_API | 自动配置本地 Clash API，默认 true |
| AUTO_RESTART_SING_BOX | 配置变动后自动重启服务，默认 true |
| DEFAULT_MONTHLY_LIMIT_GB | 默认每入站月额度，默认 500 GiB |
| TIMEZONE / DAILY_REPORT_HOUR | 报告时区和每日发送小时 |
| POLL_SECONDS | 流量采样周期，默认 60 秒，最小 15 秒 |

证书示例（请替换为自己的值）：

```dotenv
SHARE_DOMAIN=proxy.example.com
SHARE_HOST=proxy.example.com
CERTS_DIR=/root/certs
ANYTLS_CERT_PATH=/root/certs/fullchain.pem
ANYTLS_KEY_PATH=/root/certs/privkey.pem
```

证书文件必须位于挂载目录内，且在宿主机与容器中的路径一致。添加协议时的提示和新配置均读取上述环境变量。修改环境不会自动重写已存在的入站、出站；已有协议配置继续保存在本机。证书申请与续期由宿主机维护，本项目不自动申请或续期。

## 功能使用

- `/start`、`/panel`：主面板；`/help`：完整命令。
- `/status`、`/today`、`/month`、`/usage <tag>`：流量信息。
- `/limit <tag> <GiB>`：月额度，0 表示不限；`/block`、`/unblock`：暂停和恢复。
- `/protocols`：协议管理；`/addanytls <tag> [天数]`、`/addhy2 <tag> [天数]`、`/addvless`：新增入站；`/share`：分享链接。
- `/addout`：交互式添加 SOCKS、HTTP、AnyTLS、VLESS Reality、Hysteria 2 出站。
- `/bwh` 或搬瓦工面板：状态、流量、电源、快照等。没有配置 BWH_API_KEY / BWH_VEID 时，点击功能会提示在 `.env` 配置，不发送 API 请求。
- Telegram 频道转发助手：没有开发者 API 时提示配置 FORWARD_API_ID / FORWARD_API_HASH，不影响机器人其它功能。配置后通过私聊面板登录用户账号，按提示输入手机、验证码和二步验证密码。
- `/bind <来源> <目标>` 创建转发，目标可填 `bot`；`/switch 1` 设置规则；支持实时/轮询、黑白名单及关键词。用户会话需有访问来源、向目标发送消息的权限。

流量按实时连接采样累计，采样间隔内结束的极短连接可能遗漏；这是估算统计。额度和转发规则持久化于本地数据库。Telegram 发送与本地进度写入不能跨系统原子提交，发送后恰好宕机时可能重复转发。

## 本地私密数据与备份

`.env`、证书和私钥、sing-box 配置与备份、`data/` 中的数据库、转发规则、Telegram 会话、日志均保留在部署服务器。仓库使用允许列表 `.gitignore`，Docker 构建上下文同样只允许运行代码及依赖清单。不要强制添加被忽略的文件。

首次配置 Clash API 会在本地备份 sing-box 配置并可能重启服务。升级前请自行备份 `.env`、`data/` 和 sing-box 配置目录，备份同样不得上传。Docker 日志默认轮转 5 MB × 2。

需要清理一天前的操作和转发日志时可手动运行：

```bash
docker compose exec sbbot python maintenance.py
```

该脚本保留转发规则、会话、进度和流量数据；仓库不自动安装 systemd 定时清理任务。

## 测试

在隔离容器中运行，勿挂载生产数据：

```bash
docker build -t sbbot-test .
docker run --rm -v "$PWD:/tests:ro" -w /tests sbbot-test python -m unittest discover -v
```


## STRM 失败自动重试

打开「Telegram频道转发助手 → 切换至转发规则设置 → 选择规则」。

- 目标为另一个机器人时，用户会话接收到 `分享生成 STRM 文件失败`（忽略空白）会将对应消息加入本地重试队列，也支持编辑消息后的失败提示。
- 默认冷却 **5 分钟**，最多 **6 次额外重试**，初次发送不计入。后台每约 30 秒检查一次，因此实际发送可能稍晚；Telegram 限流、断线及长时间转发也会延后。
- 点击「转发重试」查看待重试消息：每页最多 10 条，显示文字和来源消息链接，过滤图片展示并关闭链接预览。私密频道链接需要访问权限。
- 点击「重试冷却时间」或「最多次数」，按提示发送 `/cs 5 6`。时间单位为整数分钟（1–10080），次数为 0–100；0 关闭重试。设置作用于最近选择的规则，也可先 `/switch 1` 再设置。
- 修改参数会使排队消息从修改时刻重新冷却；已达新上限的任务停止重试。已耗尽任务不会因提高上限自动恢复。
- 暂停规则也暂停其重试；恢复后处理到期任务。删除规则同时删除重试记录。队列与次数持久化于本机 `data/forwarder.db`，重启保留。
- 使用目标机器人回复引用的消息 ID 关联任务。没有引用时，仅在最近 24 小时内只有一个等待结果的候选消息时关联；存在歧义则只记录日志，避免重发错误内容。建议使用会引用原消息的目标机器人。
- 每次重试重新转发来源消息，包含原媒体及需要补发的展开文字；「过滤图片」仅作用于队列展示。源消息删除、访问权限丢失和发送异常也消耗重试次数，达到上限停止。
- 目标填 `bot` 的自有管理机器人展示模式不接收外部机器人 STRM 处理回复；此机制适用于用户会话直接投递目标机器人的规则。
- 自动测试使用模拟失败回复，不会向真实目标机器人制造 STRM 失败。跨 Telegram 和数据库的发送无法保证严格一次，进程在发送期间退出可能导致重复或结果不确定。
