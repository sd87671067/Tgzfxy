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
