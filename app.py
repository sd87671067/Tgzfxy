import asyncio
import hashlib
import secrets
import json
import logging
import os
import sqlite3
import subprocess
from datetime import datetime, timedelta
from pathlib import Path
from zoneinfo import ZoneInfo

import aiohttp
import base64
import urllib.parse
from telegram import Update, InlineKeyboardButton, InlineKeyboardMarkup
from telegram.ext import Application, CommandHandler, CallbackQueryHandler, ContextTypes, MessageHandler, filters

CONFIG = Path("/config/config.json")
DATA = Path("/data/sbbot.db")
MANAGED_BLOCK = "__sbbot_monthly_limit_block__"


def env_bool(name: str, default: bool) -> bool:
    return os.getenv(name, str(default)).lower() in {"1", "true", "yes", "on"}


TZ = ZoneInfo(os.getenv("TIMEZONE", "Asia/Shanghai"))
DEFAULT_LIMIT = int(float(os.getenv("DEFAULT_MONTHLY_LIMIT_GB", "500")) * 1024 ** 3)
POLL_SECONDS = max(15, int(os.getenv("POLL_SECONDS", "60")))
API_URL = os.getenv("SINGBOX_API_URL", "http://127.0.0.1:9090").rstrip("/")
API_SECRET = os.getenv("SINGBOX_API_SECRET", "")
def parse_admins(value):
    try:
        ids = {int(x.strip()) for x in value.split(",") if x.strip()}
        return ids if all(x > 0 for x in ids) else set()
    except ValueError:
        return set()


ADMINS = parse_admins(os.getenv("TELEGRAM_ADMIN_IDS", ""))
BWH_API = "https://api.64clouds.com/v1"
BWH_KEY = os.getenv("BWH_API_KEY", "")
BWH_VEID = os.getenv("BWH_VEID", "")
SHARE_HOST = os.getenv("SHARE_HOST", "")
SHARE_DOMAIN = os.getenv("SHARE_DOMAIN", "").strip()
REALITY_SNI = os.getenv("REALITY_SNI", "itunes.apple.com")
ANYTLS_CERT_PATH = os.getenv("ANYTLS_CERT_PATH", "").strip()
ANYTLS_KEY_PATH = os.getenv("ANYTLS_KEY_PATH", "").strip()
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
# httpx logs complete request URLs; Telegram embeds the bot token in that URL.
logging.getLogger("httpx").setLevel(logging.WARNING)


def tls_config_hint():
    return (f"域名：{SHARE_DOMAIN or '未设置 SHARE_DOMAIN'}\n"
            f"证书：{ANYTLS_CERT_PATH or '未设置 ANYTLS_CERT_PATH'}\n"
            f"私钥：{ANYTLS_KEY_PATH or '未设置 ANYTLS_KEY_PATH'}\n"
            "在 .env 中修改以上配置后，运行 docker compose up -d 重新创建容器。")


class Store:
    def __init__(self):
        DATA.parent.mkdir(parents=True, exist_ok=True)
        self.db = sqlite3.connect(DATA)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.executescript("""
        CREATE TABLE IF NOT EXISTS samples(ts INTEGER NOT NULL, tag TEXT NOT NULL, bytes INTEGER NOT NULL);
        CREATE TABLE IF NOT EXISTS connections(id TEXT PRIMARY KEY, tag TEXT NOT NULL, total INTEGER NOT NULL);
        CREATE TABLE IF NOT EXISTS limits(tag TEXT PRIMARY KEY, bytes INTEGER NOT NULL);
        CREATE TABLE IF NOT EXISTS reports(day TEXT PRIMARY KEY);
        CREATE TABLE IF NOT EXISTS opslog(id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, actor TEXT, action TEXT, detail TEXT);
        CREATE TABLE IF NOT EXISTS proto_expiry(tag TEXT PRIMARY KEY, expires_at INTEGER);
        """)
        self.db.commit()

    def add(self, tag, amount):
        if amount > 0:
            self.db.execute("INSERT INTO samples VALUES(?,?,?)", (int(datetime.now().timestamp()), tag, amount))
            self.db.commit()

    def usage(self, tag, start):
        return self.db.execute("SELECT COALESCE(SUM(bytes),0) FROM samples WHERE tag=? AND ts>=?", (tag, int(start.timestamp()))).fetchone()[0]

    def usage_all(self, start):
        rows = self.db.execute("SELECT tag, COALESCE(SUM(bytes),0) FROM samples WHERE ts>=? GROUP BY tag ORDER BY 2 DESC", (int(start.timestamp()),)).fetchall()
        return {t: b for t, b in rows}

    def limit(self, tag):
        row = self.db.execute("SELECT bytes FROM limits WHERE tag=?", (tag,)).fetchone()
        return row[0] if row else DEFAULT_LIMIT

    def set_limit(self, tag, value):
        self.db.execute("INSERT INTO limits(tag,bytes) VALUES(?,?) ON CONFLICT(tag) DO UPDATE SET bytes=excluded.bytes", (tag, value))
        self.db.commit()

    def set_expiry(self, tag, expires_at: int | None):
        if expires_at is None:
            self.db.execute("DELETE FROM proto_expiry WHERE tag=?", (tag,))
        else:
            self.db.execute("INSERT INTO proto_expiry(tag,expires_at) VALUES(?,?) ON CONFLICT(tag) DO UPDATE SET expires_at=excluded.expires_at", (tag, int(expires_at)))
        self.db.commit()

    def expiry(self, tag):
        row = self.db.execute("SELECT expires_at FROM proto_expiry WHERE tag=?", (tag,)).fetchone()
        return row[0] if row else None

    def expiries(self):
        return {t: e for t, e in self.db.execute("SELECT tag,expires_at FROM proto_expiry").fetchall()}

    def clear_expiry(self, tag):
        self.db.execute("DELETE FROM proto_expiry WHERE tag=?", (tag,))
        self.db.commit()

    def log_op(self, actor, action, detail=""):
        self.db.execute("INSERT INTO opslog(ts,actor,action,detail) VALUES(?,?,?,?)", (int(datetime.now().timestamp()), str(actor), action, detail))
        self.db.commit()

    def ops(self, limit=20):
        return self.db.execute("SELECT ts,actor,action,detail FROM opslog ORDER BY id DESC LIMIT ?", (limit,)).fetchall()


store = Store()


def load_config():
    with CONFIG.open() as f:
        return json.load(f)


def inbound_map():
    try:
        return {str(x["tag"]): x.get("listen_port") for x in load_config().get("inbounds", []) if x.get("tag")}
    except Exception:
        logging.exception("Unable to read sing-box config")
        return {}


def parse_socks5_input(text: str) -> dict:
    aliases = {
        "proxy server": "server", "server": "server", "host": "server", "address": "server",
        "port": "server_port", "username": "username", "user": "username", "password": "password", "pass": "password",
    }
    parsed = {}
    for raw in text.splitlines():
        if ":" not in raw:
            continue
        key, value = raw.split(":", 1)
        canonical = aliases.get(key.strip().lower())
        if canonical:
            parsed[canonical] = value.strip()
    required = ("server", "server_port", "username", "password")
    missing = [k for k in required if not parsed.get(k)]
    if missing:
        raise ValueError("缺少字段: " + ", ".join(missing))
    try:
        parsed["server_port"] = int(parsed["server_port"])
    except ValueError as exc:
        raise ValueError("port 必须是整数") from exc
    if not 1 <= parsed["server_port"] <= 65535:
        raise ValueError("port 必须在 1-65535")
    return parsed


def parse_outbound_input(text: str) -> dict:
    """Parse a SOCKS form or a standard proxy URI without guessing missing secrets."""
    import re
    import uuid
    text = text.strip()
    if "://" not in text:
        return {"type": "socks", **parse_socks5_input(text)}
    if any(c.isspace() for c in text):
        raise ValueError("请一次发送一个链接，空格请使用 URL 编码")
    u = urllib.parse.urlsplit(text)
    scheme = u.scheme.lower()
    kind = {"hy2": "hysteria2", "hysteria2": "hysteria2", "anytls": "anytls", "vless": "vless", "socks": "socks", "socks5": "socks", "http": "http", "https": "http"}.get(scheme)
    if not kind:
        raise ValueError("支持 SOCKS/HTTP、AnyTLS、VLESS Reality、Hysteria 2 链接")
    try:
        port = u.port or (80 if scheme == "http" else 443)
    except ValueError as exc:
        raise ValueError("端口必须在 1-65535") from exc
    if not u.hostname or not 1 <= port <= 65535 or (':' in u.netloc.rsplit('@', 1)[-1] and u.port == 0):
        raise ValueError("缺少服务器地址或端口无效")
    if kind == "socks" and u.port is None:
        raise ValueError("SOCKS5 链接必须指定端口")
    q = dict(urllib.parse.parse_qsl(u.query, keep_blank_values=True))
    out = {"type": kind, "server": u.hostname, "server_port": port}
    auth = urllib.parse.unquote(u.netloc.rsplit('@', 1)[0]) if '@' in u.netloc else ''
    if kind in {"socks", "http"}:
        if scheme == "https":
            out["tls"] = {"enabled": True, "server_name": u.hostname}
        if u.username is not None:
            out.update(username=urllib.parse.unquote(u.username), password=urllib.parse.unquote(u.password or ''))
        return out
    if not auth:
        raise ValueError("链接缺少密码或 UUID")
    tls = {"enabled": True, "server_name": q.get("sni") or q.get("peer") or q.get("serverName") or u.hostname}
    insecure = q.get("insecure", q.get("allowInsecure", "0")).lower()
    if insecure not in {"0", "1", "true", "false"}:
        raise ValueError("insecure 必须为 0/1/true/false")
    tls["insecure"] = insecure in {"1", "true"}
    if q.get("alpn"):
        tls["alpn"] = q["alpn"].split(',')
    out["tls"] = tls
    if kind == "vless":
        try:
            out["uuid"] = str(uuid.UUID(auth))
        except ValueError as exc:
            raise ValueError("VLESS UUID 无效") from exc
        if q.get("security") != "reality":
            raise ValueError("VLESS 目前支持 security=reality")
        if q.get("type", "tcp") not in {"tcp", "raw"}:
            raise ValueError("VLESS Reality 目前支持 TCP/raw 传输")
        pbk = q.get("pbk", "")
        try:
            if not re.fullmatch(r"[A-Za-z0-9_-]{43}", pbk) or len(base64.urlsafe_b64decode(pbk + '=')) != 32:
                raise ValueError()
        except Exception as exc:
            raise ValueError("Reality 缺少有效 pbk 公钥") from exc
        sid = q.get("sid", "")
        if not re.fullmatch(r"(?:[0-9a-fA-F]{2}){0,8}", sid):
            raise ValueError("Reality sid 必须为至多 16 位、偶数长度的十六进制")
        flow = q.get("flow", "")
        if flow not in {"", "xtls-rprx-vision"}:
            raise ValueError("不支持的 VLESS flow")
        if flow:
            out["flow"] = flow
        tls["insecure"] = False
        tls["utls"] = {"enabled": True, "fingerprint": q.get("fp") or "chrome"}
        tls["reality"] = {"enabled": True, "public_key": pbk, "short_id": sid}
    else:
        out["password"] = auth
        if kind == "hysteria2" and q.get("obfs"):
            if q["obfs"] != "salamander" or not q.get("obfs-password"):
                raise ValueError("Hysteria 2 混淆需 obfs=salamander 和 obfs-password")
            out["obfs"] = {"type": "salamander", "password": q["obfs-password"]}
        if kind == "hysteria2" and q.get("mport"):
            ports = q["mport"].replace('-', ':').split(',')
            for entry in ports:
                bounds = entry.split(':')
                if len(bounds) > 2 or not all(v.isdigit() and 1 <= int(v) <= 65535 for v in bounds) or int(bounds[0]) > int(bounds[-1]):
                    raise ValueError("mport 端口范围无效")
            out.pop("server_port")
            out["server_ports"] = ports
    return out


def add_outbound_to_doc(doc: dict, values: dict) -> str:
    return add_socks_outbound_to_doc(doc, values, tag_factory=lambda: values.get("type", "socks") + "-" + os.urandom(4).hex())


def random_socks_tag() -> str:
    return "socks-" + os.urandom(4).hex()


def add_socks_outbound_to_doc(doc: dict, values: dict, tag_factory=random_socks_tag) -> str:
    existing = {x.get("tag") for x in doc.setdefault("outbounds", [])}
    for _ in range(20):
        tag = tag_factory()
        if tag not in existing:
            doc["outbounds"].append({"type": "socks", "tag": tag, **values})
            return tag
    raise RuntimeError("无法生成唯一出站 tag")


def selection_kind(doc: dict, tag: str) -> str | None:
    if any(x.get("tag") == tag for x in doc.get("inbounds", [])):
        return "inbound"
    if any(x.get("tag") == tag for x in doc.get("outbounds", [])):
        return "outbound"
    return None


def validate_binding_selection(doc: dict, first: str, second: str) -> tuple[str, str]:
    first_kind, second_kind = selection_kind(doc, first), selection_kind(doc, second)
    if {first_kind, second_kind} != {"inbound", "outbound"}:
        raise ValueError("必须选择一个入站 tag 和一个出站 tag，不能选择两个入站或两个出站")
    return (first, second) if first_kind == "inbound" else (second, first)


def bind_route_in_doc(doc: dict, inbound_tag: str, outbound_tag: str) -> None:
    validate_binding_selection(doc, inbound_tag, outbound_tag)
    route = doc.setdefault("route", {})
    rules = route.setdefault("rules", [])
    block_rules, remaining = [], []
    for rule in rules:
        if rule.get("outbound") == MANAGED_BLOCK:
            block_rules.append(rule)
        elif inbound_tag in rule.get("inbound", []):
            leftovers = [x for x in rule.get("inbound", []) if x != inbound_tag]
            if leftovers:
                new_rule = dict(rule)
                new_rule["inbound"] = leftovers
                remaining.append(new_rule)
        else:
            remaining.append(rule)
    route["rules"] = block_rules + [{"inbound": [inbound_tag], "outbound": outbound_tag}] + remaining


def restart_singbox():
    if not env_bool("AUTO_RESTART_SING_BOX", True):
        return True, "已修改配置；AUTO_RESTART_SING_BOX=false，未重启 sing-box"
    try:
        check = subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/local/bin/sing-box", "check", "-c", "/etc/sing-box/config.json"], capture_output=True, text=True, timeout=30)
        if check.returncode:
            return False, "配置校验失败：" + (check.stderr or check.stdout)[-500:]
        run = subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/bin/systemctl", "restart", "sing-box"], capture_output=True, text=True, timeout=45)
        return run.returncode == 0, (run.stderr or run.stdout or "sing-box 已重启").strip()
    except Exception as e:
        return False, str(e)


def write_config(doc):
    backup = CONFIG.with_name(f"config.json.sbbot-backup-{datetime.now():%Y%m%d%H%M%S}")
    if not backup.exists():
        backup.write_bytes(CONFIG.read_bytes())
    temp = CONFIG.with_suffix(".json.sbbot-tmp")
    temp.write_text(json.dumps(doc, ensure_ascii=False, indent=2) + "\n")
    temp.replace(CONFIG)


def ensure_api():
    if not env_bool("AUTO_CONFIGURE_CLASH_API", True):
        return
    doc = load_config()
    exp = doc.setdefault("experimental", {})
    clash = exp.setdefault("clash_api", {})
    expected = {"external_controller": "127.0.0.1:9090", "secret": API_SECRET}
    if any(clash.get(k) != v for k, v in expected.items()):
        clash.update(expected)
        write_config(doc)
        ok, msg = restart_singbox()
        if not ok:
            raise RuntimeError(msg)
        logging.info("Clash API configured: %s", msg)


def blocked_tags():
    return {
        tag
        for rule in load_config().get("route", {}).get("rules", [])
        if rule.get("outbound") == MANAGED_BLOCK
        for tag in rule.get("inbound", [])
    }


def set_blocked(tags: set[str]):
    doc = load_config()
    outbounds = doc.setdefault("outbounds", [])
    if not any(x.get("tag") == MANAGED_BLOCK for x in outbounds):
        outbounds.append({"type": "block", "tag": MANAGED_BLOCK})
    route = doc.setdefault("route", {})
    rules = [x for x in route.get("rules", []) if x.get("outbound") != MANAGED_BLOCK]
    if tags:
        rules.insert(0, {"inbound": sorted(tags), "outbound": MANAGED_BLOCK})
    if rules == route.get("rules", []):
        return True, "额度状态未变化"
    original = json.loads(json.dumps(doc))
    route["rules"] = rules
    write_config(doc)
    ok, msg = restart_singbox()
    if not ok:
        write_config(original)
        restart_singbox()
        return False, "配置失败，已回滚：" + msg
    return True, msg


def month_start(now):
    return now.replace(day=1, hour=0, minute=0, second=0, microsecond=0)


def day_start(now):
    return now.replace(hour=0, minute=0, second=0, microsecond=0)


async def collect_once(session):
    headers = {"Authorization": f"Bearer {API_SECRET}"} if API_SECRET else {}
    async with session.get(API_URL + "/connections", headers=headers, timeout=aiohttp.ClientTimeout(total=8)) as r:
        r.raise_for_status()
        payload = await r.json()
    valid = set(inbound_map())
    live = set()
    for c in payload.get("connections", []):
        meta = c.get("metadata") or {}
        tag = meta.get("inbound")
        if not tag:
            connection_type = str(meta.get("type", ""))
            _, separator, candidate = connection_type.rpartition("/")
            tag = candidate if separator else None
        cid = str(c.get("id", ""))
        if not cid or tag not in valid:
            continue
        total = int(c.get("uploadTotal", c.get("upload", 0))) + int(c.get("downloadTotal", c.get("download", 0)))
        live.add(cid)
        previous = store.db.execute("SELECT tag,total FROM connections WHERE id=?", (cid,)).fetchone()
        if previous:
            old_tag, old_total = previous
            store.add(old_tag, max(0, total - old_total))
        store.db.execute("INSERT INTO connections(id,tag,total) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET tag=excluded.tag,total=excluded.total", (cid, tag, total))
    if live:
        store.db.execute("DELETE FROM connections WHERE id NOT IN (%s)" % ",".join("?" * len(live)), tuple(live))
    else:
        store.db.execute("DELETE FROM connections")
    store.db.commit()

    now = datetime.now(TZ)
    current = blocked_tags()
    blocked = {tag for tag in valid if store.limit(tag) > 0 and store.usage(tag, month_start(now)) >= store.limit(tag)}
    # 有效期到期自动暂停
    expired = {tag for tag in valid if is_expired(tag)}
    # 手动封锁（管理员用 /block 设置的）不被自动解封：manual_block 记录在 limits 表特殊行里
    manual = manual_blocks()
    target = blocked | expired | manual
    if target != current or not any(
        x.get("tag") == MANAGED_BLOCK for x in load_config().get("outbounds", [])
    ):
        ok, msg = set_blocked(target)
        logging.info("Limit block update %s: %s", ok, msg)
        for tag in blocked - current:
            await notify_admins(f"⛔ 入站 {tag} 本月流量已达限额 {human(store.limit(tag))}，已自动暂停。")


MANUAL_BLOCK_KEY = "__manual_block__"


def manual_blocks() -> set[str]:
    row = store.db.execute("SELECT bytes FROM limits WHERE tag=?", (MANUAL_BLOCK_KEY,)).fetchone()
    if not row:
        return set()
    return {t for t in str(row[0]).split(",") if t}


def set_manual_blocks(tags: set[str]):
    store.db.execute("INSERT INTO limits(tag,bytes) VALUES(?,?) ON CONFLICT(tag) DO UPDATE SET bytes=excluded.bytes", (MANUAL_BLOCK_KEY, ",".join(sorted(tags))))
    store.db.commit()


def human(n):
    units = ["B", "KiB", "MiB", "GiB", "TiB"]
    value = float(n)
    for u in units:
        if value < 1024 or u == units[-1]:
            return f"{value:.2f} {u}"
        value /= 1024


def resolve_target(arg):
    mapping = inbound_map()
    if arg in mapping:
        return arg
    try:
        port = int(arg)
        return next((tag for tag, p in mapping.items() if p == port), None)
    except ValueError:
        return None


def is_admin(update):
    uid = None
    if update.callback_query:
        uid = update.callback_query.from_user.id
    elif update.effective_user:
        uid = update.effective_user.id
    return uid in ADMINS


async def guarded(update):
    if is_admin(update):
        return True
    target = update.callback_query or update.effective_message
    await target.reply_text("无权限。")
    return False


# ---------------- 流量报表 ----------------

def usage_lines(period: str) -> list[str]:
    now = datetime.now(TZ)
    start = day_start(now) if period == "day" else month_start(now)
    title = "今日流量（0点起）" if period == "day" else f"本月流量（{now:%Y-%m}）"
    usage = store.usage_all(start)
    valid = inbound_map()
    rows = sorted(usage.items(), key=lambda kv: kv[1], reverse=True)
    total = sum(usage.values())
    lines = [f"📊 {title}", ""]
    for tag, n in rows:
        if tag not in valid and tag != MANUAL_BLOCK_KEY:
            continue
        mark = " ⛔" if tag in blocked_tags() else ""
        lines.append(f"{tag}：{human(n)}{mark}")
    lines.append("")
    lines.append(f"合计：{human(total)}")
    return lines


# ---------------- 搬瓦工 API ----------------

async def bwh(session, endpoint: str, params: dict | None = None, actor: str = "system", desc: str = ""):
    params = dict(params or {})
    params.update({"veid": BWH_VEID, "api_key": BWH_KEY})
    try:
        async with session.get(f"{BWH_API}/{endpoint}", params=params, timeout=aiohttp.ClientTimeout(total=60)) as r:
            # KiwiVM 返回 JSON 但 content-type 是 text/plain，必须关闭 mimetype 检查
            data = await r.json(content_type=None)
    except Exception as e:
        store.log_op(actor, f"bwh:{endpoint}", f"请求失败 {e}")
        return {"error": -1, "message": f"API 请求失败：{e}"}
    ok = data.get("error") == 0
    store.log_op(actor, f"bwh:{endpoint}", (desc or "") + (" OK" if ok else f" FAIL {data.get('message', data.get('error'))}"))
    return data


async def bwh_status_text(session) -> str:
    info = await bwh(session, "getServiceInfo")
    live = await bwh(session, "getLiveServiceInfo")
    if info.get("error") != 0:
        return f"查询失败：{info.get('message', info.get('error'))}"
    used = int(info.get("data_counter", 0))
    total = int(info.get("plan_monthly_data", 0))
    reset = datetime.fromtimestamp(int(info.get("data_next_reset", 0)), TZ)
    lines = [
        f"🖥 {info.get('hostname', '?')}（{info.get('node_location', '?')}）",
        f"系统：{info.get('os', '?')}｜类型：{info.get('vm_type', '?')}",
        f"状态：{'⏸ 已停机' if info.get('suspended') else ('✅ 运行中' if live.get('ve_status') == 'Running' else (live.get('ve_status') or '运行中'))}",
        f"负载：{live.get('load_average', '?')}",
        f"内存可用：{int(live.get('mem_available_kb', 0) / 1024):.1f} MiB",
        f"流量：{human(used)} / {human(total)}（{used * 100 // total if total else 0}%）",
        f"重置：{reset:%Y-%m-%d}",
    ]
    return "\n".join(lines)


async def bwh_transfer_text(session) -> str:
    info = await bwh(session, "getServiceInfo")
    if info.get("error") != 0:
        return f"查询失败：{info.get('message', info.get('error'))}"
    used = int(info.get("data_counter", 0))
    total = int(info.get("plan_monthly_data", 0))
    reset = datetime.fromtimestamp(int(info.get("data_next_reset", 0)), TZ)
    multiplier = info.get("monthly_data_multiplier", 1)
    remain = max(0, total - used)
    days_left = max(0, (reset - datetime.now(TZ)).days)
    lines = [
        "📶 搬瓦工本轮流量（计费周期）",
        f"已用：{human(used)}",
        f"总量：{human(total)}",
        f"剩余：{human(remain)}（{remain * 100 // total if total else 0}%）",
        f"重置时间：{reset:%Y-%m-%d %H:%M}（还剩 {days_left} 天）",
        f"计量倍率：×{multiplier}",
        f"日均可用：{human(remain // days_left if days_left else remain)}",
    ]
    return "\n".join(lines)


# ---------------- 分享链接 ----------------

def x25519_public(private_key_b64: str) -> str:
    raw = base64.urlsafe_b64decode(private_key_b64 + "=" * (-len(private_key_b64) % 4))
    from cryptography.hazmat.primitives.asymmetric.x25519 import X25519PrivateKey
    from cryptography.hazmat.primitives import serialization
    pub = X25519PrivateKey.from_private_bytes(raw).public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    return base64.urlsafe_b64encode(pub).decode().rstrip("=")


def server_host() -> str:
    if SHARE_HOST:
        return SHARE_HOST
    try:
        out = subprocess.run(["ip", "-4", "-o", "addr", "show", "scope", "global"], capture_output=True, text=True, timeout=10).stdout
        for line in out.splitlines():
            parts = line.split()
            if len(parts) >= 4 and not parts[3].startswith("127.") and not parts[3].startswith("172."):
                return parts[3].split("/")[0]
    except Exception:
        pass
    return "127.0.0.1"


# ---------------- 协议入站管理 ----------------

def random_port(used: set[int]) -> int:
    import secrets
    import socket
    for _ in range(256):
        p = 10000 + secrets.randbelow(52001)
        if p in used:
            continue
        try:
            with socket.socket(socket.AF_INET6, socket.SOCK_STREAM) as tcp, socket.socket(socket.AF_INET6, socket.SOCK_DGRAM) as udp:
                tcp.bind(("::", p))
                udp.bind(("::", p))
            return p
        except OSError:
            continue
    raise RuntimeError("无法找到空闲随机端口")


def existing_ports() -> set[int]:
    doc = load_config()
    return {x.get("listen_port") for x in doc.get("inbounds", []) if x.get("listen_port")}


def gen_reality_keypair() -> tuple[str, str]:
    r = subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/local/bin/sing-box", "generate", "reality-keypair"], capture_output=True, text=True, timeout=30)
    if r.returncode != 0:
        raise RuntimeError(r.stderr or "reality-keypair 生成失败")
    private = public = ""
    for line in r.stdout.splitlines():
        if "PrivateKey" in line:
            private = line.split(":", 1)[1].strip()
        elif "PublicKey" in line:
            public = line.split(":", 1)[1].strip()
    return private, public


def gen_uuid() -> str:
    import uuid
    return str(uuid.uuid4())


def gen_anytls_password() -> str:
    import base64 as _b64
    return _b64.urlsafe_b64encode(os.urandom(18)).decode().rstrip("=")


def parse_duration_days(text: str) -> int | None:
    """解析有效期字符串为天数；0/空/permanent/none 返回 None（永久）。支持：纯天数、y/m/d/w 复合单位。"""
    if text is None:
        return None
    text = str(text).strip()
    if not text:
        return None
    if text.lower() in {"0", "permanent", "none", "never", "forever", "永久", "无期限"}:
        return None
    import re
    total = 0
    for num, unit in re.findall(r"(\d+(?:\.\d+)?)\s*(y|m|d|w)", text, flags=re.IGNORECASE):
        n = float(num)
        u = unit.lower()
        total += n * {"y": 365, "m": 30, "w": 7, "d": 1}[u]
    if total > 0:
        days = int(total)
        return days if days > 0 else None
    try:
        days = int(float(text))
    except ValueError:
        raise ValueError(f"无法解析有效期「{text}」：请用数字天数（如 30）或组合单位（如 1y 或 3m 或 2w 或 7d）")
    return days if days > 0 else None


def expiry_timestamp(days: int | None) -> int | None:
    if days is None:
        return None
    return int((datetime.now(TZ) + timedelta(days=days)).timestamp())


def expiry_text(tag: str, ts: int | None) -> str:
    if not ts:
        return "永久"
    dt = datetime.fromtimestamp(ts, TZ)
    if dt <= datetime.now(TZ):
        return f"{dt:%Y-%m-%d}（已过期）"
    return dt.strftime("%Y-%m-%d")


def is_expired(tag: str) -> bool:
    ts = store.expiry(tag)
    return bool(ts and ts <= int(datetime.now(TZ).timestamp()))


def add_anytls_inbound(tag: str, expires_days: int | None = None) -> tuple[bool, str]:
    """添加 AnyTLS 入站，域名和证书路径由环境配置。"""
    doc = load_config()
    if any(x.get("tag") == tag for x in doc.get("inbounds", [])):
        return False, f"tag {tag} 已存在"
    cert, key = ANYTLS_CERT_PATH, ANYTLS_KEY_PATH
    if not SHARE_DOMAIN or not cert or not key:
        return False, "请在 .env 中设置 SHARE_DOMAIN、ANYTLS_CERT_PATH、ANYTLS_KEY_PATH，之后重新创建容器。"
    if not (Path(cert).is_file() and Path(key).is_file()):
        return False, f"证书文件不存在：需 {cert} 和 {key}"
    used = existing_ports()
    port = random_port(used)
    password = gen_anytls_password()
    inbound = {
        "type": "anytls",
        "tag": tag,
        "listen": "::",
        "listen_port": port,
        "users": [{"name": tag, "password": password}],
        "tls": {"enabled": True, "server_name": SHARE_DOMAIN, "certificate_path": cert, "key_path": key},
    }
    doc["inbounds"].append(inbound)
    write_config(doc)
    ok, msg = restart_singbox()
    if not ok:
        doc["inbounds"] = [x for x in doc["inbounds"] if x.get("tag") != tag]
        write_config(doc)
        restart_singbox()
        return False, f"重启失败已回滚：{msg}"
    firewall = subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/sbin/ufw", "allow", str(port), "comment", f"sbbot-anytls-{tag}"], capture_output=True, text=True, timeout=30)
    if firewall.returncode != 0:
        doc["inbounds"] = [x for x in doc["inbounds"] if x.get("tag") != tag]
        write_config(doc)
        restart_singbox()
        return False, f"UFW 放行失败已回滚：{firewall.stderr or firewall.stdout}"
    store.set_expiry(tag, expiry_timestamp(expires_days))
    host = SHARE_HOST or server_host()
    params = urllib.parse.urlencode({"sni": SHARE_DOMAIN, "insecure": "0"})
    info = (f"tag={tag} 端口={port}\npassword={password}\nsni={SHARE_DOMAIN}\n"
            f"Shadowrocket：服务器地址填 {host}，TLS SNI 填 {SHARE_DOMAIN}；开启证书验证。\n"
            f"链接：anytls://{urllib.parse.quote(password, safe='')}@{host}:{port}/?{params}#{urllib.parse.quote(tag, safe='')}"
            + ("" if expires_days is None else f"\n有效期：{expires_days} 天"))
    return True, info


def add_hysteria2_inbound(tag: str, expires_days: int | None = None) -> tuple[bool, str]:
    import re
    if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", tag):
        return False, "tag 仅支持 1-64 位英文、数字、下划线和连字符"
    doc = load_config()
    if any(x.get("tag") == tag for x in doc.get("inbounds", [])):
        return False, f"tag {tag} 已存在"
    port = random_port({x.get("listen_port") for x in doc.get("inbounds", [])})
    password = gen_anytls_password()
    cert, key = ANYTLS_CERT_PATH, ANYTLS_KEY_PATH
    if not SHARE_DOMAIN or not cert or not key:
        return False, "请在 .env 中设置 SHARE_DOMAIN、ANYTLS_CERT_PATH、ANYTLS_KEY_PATH，之后重新创建容器。"
    if not (Path(cert).is_file() and Path(key).is_file()):
        return False, f"证书文件不存在：需 {cert} 和 {key}"
    import copy
    original = copy.deepcopy(doc)
    doc.setdefault("inbounds", []).append({"type": "hysteria2", "tag": tag, "listen": "::", "listen_port": port,
        "users": [{"name": tag, "password": password}],
        "tls": {"enabled": True, "server_name": SHARE_DOMAIN, "certificate_path": cert, "key_path": key}})
    try:
        write_config(doc)
        ok, msg = restart_singbox()
        if not ok:
            raise RuntimeError(msg)
        firewall = subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/sbin/ufw", "allow", f"{port}/udp", "comment", f"sbbot-hy2-{tag}"], capture_output=True, text=True, timeout=30)
        if firewall.returncode:
            raise RuntimeError(firewall.stderr or firewall.stdout)
        store.set_expiry(tag, expiry_timestamp(expires_days))
    except Exception as exc:
        write_config(original)
        rollback_ok, rollback_msg = restart_singbox()
        return False, f"添加失败：{exc}；配置回滚：{rollback_ok} {rollback_msg}"
    host = SHARE_HOST or server_host()
    link = f"hysteria2://{urllib.parse.quote(password, safe='')}@{host}:{port}/?sni={SHARE_DOMAIN}&insecure=0#{urllib.parse.quote(tag, safe='')}"
    return True, f"tag={tag} UDP端口={port}\n证书：{SHARE_DOMAIN}，请在宿主机维护证书续期\n客户端开启证书验证（insecure=0）。\n链接：{link}\n账户有效期：{expiry_text(tag, store.expiry(tag))}"


def add_vless_reality_inbound(tag: str, sni: str | None = None, expires_days: int | None = None) -> tuple[bool, str]:
    doc = load_config()
    if any(x.get("tag") == tag for x in doc.get("inbounds", [])):
        return False, f"tag {tag} 已存在"
    used = existing_ports()
    port = random_port(used)
    private, public = gen_reality_keypair()
    uuid = gen_uuid()
    sni = (sni or REALITY_SNI).strip()
    handshake = sni
    short_id = os.urandom(4).hex()
    inbound = {
        "type": "vless",
        "tag": tag,
        "listen": "::",
        "listen_port": port,
        "users": [{"uuid": uuid, "flow": "xtls-rprx-vision"}],
        "tls": {
            "enabled": True,
            "server_name": sni,
            "reality": {
                "enabled": True,
                "handshake": {"server": handshake, "server_port": 443},
                "private_key": private,
                "short_id": [short_id],
            },
        },
    }
    doc["inbounds"].append(inbound)
    write_config(doc)
    ok, msg = restart_singbox()
    if not ok:
        # 回滚
        doc["inbounds"] = [x for x in doc["inbounds"] if x.get("tag") != tag]
        write_config(doc)
        restart_singbox()
        return False, f"重启失败已回滚：{msg}"
    # 同步放行随机分配的 TCP 端口；失败时撤销入站，避免生成不可用节点
    firewall = subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/sbin/ufw", "allow", str(port), "comment", f"sbbot-vless-{tag}"], capture_output=True, text=True, timeout=30)
    if firewall.returncode != 0:
        doc["inbounds"] = [x for x in doc["inbounds"] if x.get("tag") != tag]
        write_config(doc)
        restart_singbox()
        return False, f"UFW 放行失败已回滚：{firewall.stderr or firewall.stdout}"
    store.set_expiry(tag, expiry_timestamp(expires_days))
    info = (f"tag={tag} 端口={port} uuid={uuid}\nsni={sni} pbk={public} sid={short_id}"
            + ("" if expires_days is None else f"\n有效期：{expires_days} 天"))
    return True, info


def delete_tag_from_doc(doc: dict, kind: str, tag: str) -> int | None:
    if kind not in {"inbound", "outbound"}:
        raise ValueError("类型必须是 inbound 或 outbound")
    if kind == "outbound" and tag in {"direct", MANAGED_BLOCK}:
        raise ValueError(f"系统出站 {tag} 不允许删除")
    collection = "inbounds" if kind == "inbound" else "outbounds"
    target = next((x for x in doc.get(collection, []) if x.get("tag") == tag), None)
    if not target:
        raise ValueError(f"{kind} tag {tag} 不存在")
    doc[collection] = [x for x in doc.get(collection, []) if x.get("tag") != tag]
    route = doc.setdefault("route", {})
    new_rules = []
    if kind == "inbound":
        for rule in route.get("rules", []):
            if tag not in rule.get("inbound", []):
                new_rules.append(rule)
                continue
            leftovers = [x for x in rule.get("inbound", []) if x != tag]
            if leftovers:
                updated = dict(rule)
                updated["inbound"] = leftovers
                new_rules.append(updated)
    else:
        for rule in route.get("rules", []):
            if rule.get("outbound") == tag:
                updated = dict(rule)
                updated["outbound"] = "direct"
                new_rules.append(updated)
            else:
                new_rules.append(rule)
    route["rules"] = new_rules
    store.clear_expiry(tag)
    return target.get("listen_port") if kind == "inbound" else None


def remove_inbound(tag: str) -> tuple[bool, str]:
    doc = load_config()
    target = next((x for x in doc["inbounds"] if x.get("tag") == tag), None)
    before = len(doc["inbounds"])
    doc["inbounds"] = [x for x in doc["inbounds"] if x.get("tag") != tag]
    if len(doc["inbounds"]) == before:
        return False, f"tag {tag} 不存在"
    write_config(doc)
    ok, msg = restart_singbox()
    if ok and target and target.get("listen_port"):
        subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/sbin/ufw", "delete", "allow", str(target["listen_port"]) + ("/udp" if target.get("type") == "hysteria2" else "")], capture_output=True, text=True, timeout=30)
    store.clear_expiry(tag)
    return ok, msg


def ufw_ports() -> list[str]:
    r = subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/sbin/ufw", "status"], capture_output=True, text=True, timeout=30)
    return [line for line in r.stdout.splitlines() if "ALLOW" in line and "(v6)" not in line]


def ufw_change(action: str, port: str, protocol: str = "both") -> tuple[bool, str]:
    if not port.isdigit() or not 1 <= int(port) <= 65535:
        return False, "端口必须是 1-65535 整数"
    cmd = ["nsenter", "-t", "1", "-m", "-p", "--", "/usr/sbin/ufw", action]
    if action == "allow":
        cmd.append(f"{port}/{protocol}" if protocol != "both" else port)
    else:
        cmd.extend(["allow", f"{port}/{protocol}" if protocol != "both" else port])
    r = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
    return r.returncode == 0, (r.stdout or r.stderr).strip()


def share_links(tags: list[str]) -> tuple[str, list[str]]:
    """返回 (v2rayN 剪贴板订阅串, 错误列表)"""
    doc = load_config()
    host = server_host()
    links, errors = [], []
    for tag in tags:
        inbound = next((x for x in doc.get("inbounds", []) if x.get("tag") == tag), None)
        if not inbound:
            errors.append(tag)
            continue
        users = inbound.get("users") or []
        if not users:
            errors.append(f"{tag}(无用户)")
            continue
        user = users[0]
        tls = inbound.get("tls") or {}
        if inbound.get("type") == "hysteria2":
            params = urllib.parse.urlencode({"sni": tls.get("server_name") or SHARE_DOMAIN, "insecure": "0"})
            links.append(f"hysteria2://{urllib.parse.quote(str(user.get('password', '')), safe='')}@{host}:{inbound.get('listen_port')}/?{params}#{urllib.parse.quote(tag, safe='')}")
            continue
        if inbound.get("type") == "anytls":
            password = str(user.get("password", ""))
            if not password:
                errors.append(f"{tag}(无密码)")
                continue
            params = {
                "sni": tls.get("server_name") or SHARE_DOMAIN,
                "insecure": "0",
            }
            auth = urllib.parse.quote(password, safe="")
            remark = urllib.parse.quote(tag, safe="")
            links.append(f"anytls://{auth}@{host}:{inbound.get('listen_port')}/?{urllib.parse.urlencode(params)}#{remark}")
            continue
        if inbound.get("type") != "vless":
            errors.append(f"{tag}(类型 {inbound.get('type')} 暂不支持)")
            continue
        # 参数顺序对齐 v2rayN 标准格式：
        # encryption -> flow -> security -> sni -> fp -> pbk -> sid -> type -> headerType
        params = {"encryption": "none"}
        if user.get("flow"):
            params["flow"] = user["flow"]
        if tls.get("enabled") and tls.get("reality", {}).get("enabled"):
            reality = tls["reality"]
            params["security"] = "reality"
            params["sni"] = tls.get("server_name") or reality.get("handshake", {}).get("server", "")
            params["fp"] = "chrome"
            try:
                params["pbk"] = x25519_public(reality["private_key"])
            except Exception:
                errors.append(f"{tag}(公钥派生失败)")
                continue
            sids = reality.get("short_id") or []
            if sids:
                params["sid"] = str(sids[0])
            params["type"] = "tcp"
            params["headerType"] = "none"
        elif tls.get("enabled"):
            params["security"] = "tls"
            params["sni"] = tls.get("server_name", "")
            params["fp"] = "chrome"
            params["type"] = "tcp"
            params["headerType"] = "none"
        else:
            params["type"] = "tcp"
            params["headerType"] = "none"
        query = urllib.parse.urlencode(params)
        remark = urllib.parse.quote(tag, safe="")
        links.append(f"vless://{user.get('uuid', '')}@{host}:{inbound.get('listen_port')}?{query}#{remark}")
    # v2rayN 剪贴板导入格式：所有链接 base64 一次
    blob = "\n".join(links)
    sub = base64.b64encode(blob.encode()).decode()
    return sub, errors


# ---------------- 操作面板 ----------------

def deletion_keyboard(doc: dict, selected: tuple[str, str] | None) -> InlineKeyboardMarkup:
    rows = [[InlineKeyboardButton("— 入站 tags —", callback_data="delete:noop")]]
    inbound_tags = [x.get("tag") for x in doc.get("inbounds", []) if x.get("tag")]
    for i in range(0, len(inbound_tags), 2):
        rows.append([InlineKeyboardButton(("✅ " if selected == ("inbound", t) else "") + t, callback_data=f"delete:pick:inbound:{t}") for t in inbound_tags[i:i + 2]])
    rows.append([InlineKeyboardButton("— 出站 tags —", callback_data="delete:noop")])
    outbound_tags = [x.get("tag") for x in doc.get("outbounds", []) if x.get("tag") not in {"direct", MANAGED_BLOCK}]
    for i in range(0, len(outbound_tags), 2):
        rows.append([InlineKeyboardButton(("✅ " if selected == ("outbound", t) else "") + t, callback_data=f"delete:pick:outbound:{t}") for t in outbound_tags[i:i + 2]])
    rows.append([InlineKeyboardButton("💾 保存删除", callback_data="delete:save"), InlineKeyboardButton("取消", callback_data="delete:cancel")])
    return InlineKeyboardMarkup(rows)


def binding_keyboard(doc: dict, selected: list[str]) -> InlineKeyboardMarkup:
    rows = []
    rows.append([InlineKeyboardButton("— 入站 tags —", callback_data="bind:noop")])
    inbound_tags = [x.get("tag") for x in doc.get("inbounds", []) if x.get("tag")]
    for i in range(0, len(inbound_tags), 2):
        rows.append([InlineKeyboardButton(("✅ " if t in selected else "") + t, callback_data=f"bind:pick:{t}") for t in inbound_tags[i:i + 2]])
    rows.append([InlineKeyboardButton("— 出站 tags —", callback_data="bind:noop")])
    outbound_tags = [x.get("tag") for x in doc.get("outbounds", []) if x.get("tag") and x.get("type") != "block"]
    for i in range(0, len(outbound_tags), 2):
        rows.append([InlineKeyboardButton(("✅ " if t in selected else "") + t, callback_data=f"bind:pick:{t}") for t in outbound_tags[i:i + 2]])
    rows.append([InlineKeyboardButton("💾 保存绑定", callback_data="bind:save"), InlineKeyboardButton("取消", callback_data="bind:cancel")])
    return InlineKeyboardMarkup(rows)


def inbound_list_page(page: int = 0) -> tuple[str, InlineKeyboardMarkup]:
    doc = load_config()
    inbounds = [x for x in doc.get("inbounds", []) if x.get("tag")]
    per = 10
    total_pages = max(1, (len(inbounds) + per - 1) // per)
    page = max(0, min(page, total_pages - 1))
    chunk = inbounds[page * per:(page + 1) * per]
    lines = [f"📋 全部入站协议（共 {len(inbounds)} 个，第 {page + 1}/{total_pages} 页）", ""]
    for x in chunk:
        tag = x.get("tag")
        ptype = x.get("type")
        port = x.get("listen_port", "")
        exp = expiry_text(tag, store.expiry(tag))
        lim = store.limit(tag)
        quota = "不限" if lim == 0 else human(lim)
        expired = " ⛔" if is_expired(tag) else ""
        lines.append(f"{tag}｜{ptype}｜:{port}｜{quota}｜有效期 {exp}{expired}")
    nav = []
    if page > 0:
        nav.append(InlineKeyboardButton("◀ 上一页", callback_data=f"proto:list:{page - 1}"))
    if page < total_pages - 1:
        nav.append(InlineKeyboardButton("下一页 ▶", callback_data=f"proto:list:{page + 1}"))
    rows = [nav] if nav else []
    rows.append([InlineKeyboardButton("« 返回协议管理", callback_data="proto:menu")])
    return "\n".join(lines), InlineKeyboardMarkup(rows)


def proto_menu_keyboard():
    return InlineKeyboardMarkup([
        [InlineKeyboardButton("➕ 添加Hysteria 2入站", callback_data="proto:hy2")],
        [InlineKeyboardButton("➕ 添加VLESS入站", callback_data="proto:vless"),
         InlineKeyboardButton("➕ 添加AnyTLS入站", callback_data="proto:anytls")],
        [InlineKeyboardButton("➕ 添加出站", callback_data="out:add"),
         InlineKeyboardButton("🔀 入站绑定出站", callback_data="bind:start")],
        [InlineKeyboardButton("🗑 删除入站/出站", callback_data="delete:start"),
         InlineKeyboardButton("🔗 转化链接", callback_data="share")],
        [InlineKeyboardButton("📋 查看所有入站", callback_data="proto:list:0"),
         InlineKeyboardButton("⏳ 设置有效期", callback_data="proto:exp")],
        [InlineKeyboardButton("« 返回面板", callback_data="panel")],
    ])


def cancel_keyboard():
    return InlineKeyboardMarkup([[InlineKeyboardButton("✖️ 取消操作", callback_data="cancel")]])


def clear_pending(context):
    login = context.user_data.get("fw_login")
    fw = getattr(context, "bot_data", {}).get("forwarder")
    if login and fw and fw.login_owner == login.get("owner"):
        fw.login_owner, fw.login_until = None, 0
    for key in ("awaiting_socks_outbound", "bind_selection", "delete_selection", "share_tags", "fw_login", "fw_keywords", "snapshot_delete_pending"):
        context.user_data.pop(key, None)


def panel_keyboard():
    return InlineKeyboardMarkup([
        [InlineKeyboardButton("📊 今日流量", callback_data="use:day"),
         InlineKeyboardButton("📅 本月流量", callback_data="use:month")],
        [InlineKeyboardButton("🚦 限额管理", callback_data="limits"),
         InlineKeyboardButton("🚧 封禁管理", callback_data="blocks")],
        [InlineKeyboardButton("🔐 协议管理", callback_data="proto:menu")],
        [InlineKeyboardButton("🧱 UFW端口管理", callback_data="ufw:list")],
        [InlineKeyboardButton("🖥 服务器状态", callback_data="bwh:status"),
         InlineKeyboardButton("📶 本轮流量", callback_data="bwh:transfer")],
        [InlineKeyboardButton("⏻ 电源控制", callback_data="bwh:power"),
         InlineKeyboardButton("🔑 重置root密码", callback_data="bwh:resetpwd")],
        [InlineKeyboardButton("📸 快照管理", callback_data="bwh:snap")],
        [InlineKeyboardButton("📨 Telegram频道转发助手", callback_data="fw:menu")],
        [InlineKeyboardButton("📜 操作日志", callback_data="logs"), InlineKeyboardButton("✖️ 取消操作", callback_data="cancel")],
    ])


async def reply_panel(target):
    await target.reply_text("🤖 sbbot 操作面板\n点选下面的功能按钮：", reply_markup=panel_keyboard())


async def notify_admins(text: str):
    from telegram import Bot
    token = os.getenv("TELEGRAM_BOT_TOKEN")
    if not token:
        return
    bot = Bot(token)
    try:
        for admin in ADMINS:
            try:
                await bot.send_message(admin, text)
            except Exception:
                logging.exception("notify admin %s failed", admin)
    finally:
        await bot.session.close()


# ---------------- 命令 ----------------

async def cmd_panel(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    clear_pending(context)
    await reply_panel(update.effective_message)


async def cmd_today(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    await update.effective_message.reply_text("\n".join(usage_lines("day")))


async def cmd_month(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    await update.effective_message.reply_text("\n".join(usage_lines("month")))


async def cmd_share(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    if not context.args:
        await update.effective_message.reply_text("用法：/share <tag1> [tag2 ...]\n例：/share zs hyx\n生成 v2rayN 可导入的分享链接。")
        return
    sub, errors = share_links(context.args)
    if errors:
        await update.effective_message.reply_text("以下 tag 无效或不支持：" + "、".join(errors))
        return
    if not sub:
        await update.effective_message.reply_text("没有生成任何链接。")
        return
    import base64 as _b64
    raw = _b64.b64decode(sub).decode()
    await update.effective_message.reply_text(f"✅ 已生成 {len(context.args)} 条链接。\n\n复制下面任意一行 → v2rayN → 服务器 → 从剪贴板导入批量URL：\n\n`{raw}`", parse_mode="Markdown")
    store.log_op(update.effective_user.id, "share", ",".join(context.args))


async def status(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    now = datetime.now(TZ); start = month_start(now); today = day_start(now)
    lines = [f"流量状态（{now:%Y-%m-%d %H:%M}）"]
    for tag, port in inbound_map().items():
        used = store.usage(tag, start); limit = store.limit(tag)
        flag = " ⛔" if limit and used >= limit else ""
        quota = "不限" if limit == 0 else human(limit)
        lines.append(f"{tag} :{port}｜今日 {human(store.usage(tag,today))}｜本月 {human(used)}/{quota}{flag}")
    await update.effective_message.reply_text("\n".join(lines) or "未发现带 tag 的入站配置。")


async def usage(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    if not context.args:
        await update.effective_message.reply_text("用法：/usage <tag 或端口>"); return
    tag = resolve_target(context.args[0])
    if not tag:
        await update.effective_message.reply_text("未找到该 tag 或端口。"); return
    now = datetime.now(TZ)
    await update.effective_message.reply_text(f"{tag}\n今日：{human(store.usage(tag, day_start(now)))}\n本月：{human(store.usage(tag, month_start(now)))}/{('不限' if store.limit(tag)==0 else human(store.limit(tag)))}")


async def limit(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    if len(context.args) != 2:
        await update.effective_message.reply_text("用法：/limit <tag或端口> <GiB>；0 为不限额"); return
    tag = resolve_target(context.args[0])
    try: value = int(float(context.args[1]) * 1024 ** 3)
    except ValueError: value = -1
    if not tag or value < 0:
        await update.effective_message.reply_text("参数无效。"); return
    store.set_limit(tag, value)
    store.log_op(update.effective_user.id, "set_limit", f"{tag} -> {'不限' if value == 0 else human(value)}")
    await update.effective_message.reply_text(f"已设置 {tag} 月限额为 {('不限' if value == 0 else human(value))}。将在下一次采集时生效。")


async def block(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    if not context.args:
        await update.effective_message.reply_text("用法：/block <tag或端口> - 手动暂停该入站"); return
    tag = resolve_target(context.args[0])
    if not tag:
        await update.effective_message.reply_text("未找到该 tag 或端口。"); return
    manual = manual_blocks(); manual.add(tag)
    set_manual_blocks(manual)
    ok, msg = set_blocked(blocked_tags() | manual)
    store.log_op(update.effective_user.id, "block", tag)
    await update.effective_message.reply_text(f"已暂停入站 {tag}：{'成功' if ok else '失败'}\n{msg}")


async def unblock(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    if not context.args:
        await update.effective_message.reply_text("用法：/unblock <tag或端口> - 解除暂停（若流量仍超限会再次被暂停）"); return
    tag = resolve_target(context.args[0])
    if not tag:
        await update.effective_message.reply_text("未找到该 tag 或端口。"); return
    manual = manual_blocks(); manual.discard(tag)
    set_manual_blocks(manual)
    # 若仍超限，下一轮采集会重新封
    now = datetime.now(TZ)
    still = store.limit(tag) > 0 and store.usage(tag, month_start(now)) >= store.limit(tag)
    target = {t for t in blocked_tags() if t != tag} | (manual if not still else {tag} | manual)
    ok, msg = set_blocked(target)
    store.log_op(update.effective_user.id, "unblock", tag)
    note = "\n注意：该 tag 本月流量仍超限，自动封锁依旧生效。" if still else ""
    await update.effective_message.reply_text(f"已解除 {tag} 的暂停：{'成功' if ok else '失败'}\n{msg}{note}")


async def cmd_addout(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    context.user_data.pop("fw_keywords", None)
    context.user_data["awaiting_socks_outbound"] = True
    await update.effective_message.reply_text(
        "请粘贴 socks://、socks5://、http://、https://、AnyTLS、VLESS Reality、Hysteria 2 链接，或以下 SOCKS5 信息：\n\n"
        "Proxy server: <服务器地址>\nport: <端口>\nusername: <用户名>\npassword: <密码>\n\n"
        "点击下方按钮或发送 /cancel 取消。", reply_markup=cancel_keyboard()
    )


async def cancel_input(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    clear_pending(context)
    await update.effective_message.reply_text("已取消当前操作。", reply_markup=panel_keyboard())


async def text_input(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not is_admin(update): return
    if await context.bot_data["forwarder"].login_input(update, context): return
    if await context.bot_data["forwarder"].keyword_input(update, context): return
    if not context.user_data.get("awaiting_socks_outbound"):
        return
    try:
        values = parse_outbound_input(update.effective_message.text or "")
    except ValueError as exc:
        await update.effective_message.reply_text(f"❌ 格式错误：{exc}\n请重新发送有效链接或 SOCKS5 四行信息，或 /cancel 取消。", reply_markup=cancel_keyboard())
        return
    doc = load_config()
    tag = add_outbound_to_doc(doc, values)
    write_config(doc)
    ok, msg = restart_singbox()
    if not ok:
        doc["outbounds"] = [x for x in doc.get("outbounds", []) if x.get("tag") != tag]
        write_config(doc)
        restart_singbox()
        await update.effective_message.reply_text(f"❌ sing-box 校验/重启失败，已回滚：{msg}")
        return
    context.user_data.pop("awaiting_socks_outbound", None)
    store.log_op(update.effective_user.id, "add_outbound", f"{tag} {values['server']}:{values.get('server_port', ','.join(values.get('server_ports', [])))}")
    await update.effective_message.reply_text(
        f"✅ {values['type']} 出站已添加\ntag：{tag}\n服务器：{values['server']}:{values.get('server_port', ','.join(values.get('server_ports', [])))}",
        reply_markup=panel_keyboard(),
    )


async def cmd_ufw(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    if not context.args or context.args[0] == "list":
        lines = ufw_ports()
        await update.effective_message.reply_text("🧱 UFW 已放行端口：\n" + ("\n".join(lines) if lines else "无" ) + "\n\n用法：/ufw allow <端口> [tcp|udp|both]\n/ufw delete <端口> [tcp|udp|both]")
        return
    if len(context.args) < 2 or context.args[0] not in {"allow", "delete"}:
        await update.effective_message.reply_text("用法：/ufw list\n/ufw allow <端口> [tcp|udp|both]\n/ufw delete <端口> [tcp|udp|both]")
        return
    protocol = context.args[2].lower() if len(context.args) > 2 else "both"
    if protocol not in {"tcp", "udp", "both"}:
        await update.effective_message.reply_text("协议只能是 tcp、udp 或 both。"); return
    ok, msg = ufw_change(context.args[0], context.args[1], protocol)
    store.log_op(update.effective_user.id, f"ufw:{context.args[0]}", f"{context.args[1]}/{protocol} {'OK' if ok else msg}")
    await update.effective_message.reply_text(("✅ " if ok else "❌ ") + msg)


async def cmd_addvless(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    args = context.args or []
    if not args:
        await update.effective_message.reply_text(
            "➕ 添加 VLESS Reality 入站\n"
            f"发送：/addvless <tag> [伪装域名] [有效期天数]\n"
            f"例：/addvless newuser\n"
            f"或：/addvless newuser itunes.apple.com 30\n\n"
            f"自动分配随机端口、生成密钥，出站直连。\n"
            f"不填伪装域名默认用 itunes.apple.com；不填有效期默认永久。\n"
            f"有效期支持数字天数，或用 1y / 3m / 2w / 7d。", reply_markup=cancel_keyboard()
        )
        return
    tag = args[0]
    sni = args[1] if len(args) > 1 and args[1] else None
    expires_days = None
    if len(args) > 2:
        try:
            expires_days = parse_duration_days(args[2])
        except ValueError as exc:
            await update.effective_message.reply_text(f"❌ {exc}")
            return
    ok, msg = add_vless_reality_inbound(tag, sni, expires_days)
    store.log_op(update.effective_user.id, "addvless", f"{tag} {'OK' if ok else msg}")
    if not ok:
        await update.effective_message.reply_text(f"❌ 添加失败：{msg}")
        return
    # 生成 vless:// 链接
    doc = load_config()
    inbound = next(x for x in doc["inbounds"] if x.get("tag") == tag)
    user = inbound["users"][0]
    reality = inbound["tls"]["reality"]
    params = {
        "encryption": "none", "flow": user["flow"], "security": "reality",
        "sni": inbound["tls"]["server_name"], "fp": "chrome",
        "pbk": x25519_public(reality["private_key"]), "sid": reality["short_id"][0],
        "type": "tcp", "headerType": "none",
    }
    link = f"vless://{user['uuid']}@{SHARE_HOST or server_host()}:{inbound['listen_port']}?{urllib.parse.urlencode(params)}#{tag}"
    await update.effective_message.reply_text(
        f"✅ VLESS Reality 入站已添加\n\n{msg}\n\n客户端链接：\n`{link}`", parse_mode="Markdown")


async def cmd_addanytls(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    args = context.args or []
    if not args:
        await update.effective_message.reply_text(
            "➕ 添加 AnyTLS 入站\n"
            "发送：/addanytls <tag> [有效期天数]\n"
            "例：/addanytls newuser\n"
            "或：/addanytls newuser 30\n\n"
            f"{tls_config_hint()}\n"
            "自动分配随机端口、生成密码，出站直连；不填有效期默认永久。", reply_markup=cancel_keyboard()
        )
        return
    tag = args[0]
    expires_days = None
    if len(args) > 1:
        try:
            expires_days = parse_duration_days(args[1])
        except ValueError as exc:
            await update.effective_message.reply_text(f"❌ {exc}")
            return
    ok, msg = add_anytls_inbound(tag, expires_days)
    store.log_op(update.effective_user.id, "addanytls", f"{tag} {'OK' if ok else msg}")
    if not ok:
        await update.effective_message.reply_text(f"❌ 添加失败：{msg}")
        return
    await update.effective_message.reply_text(f"✅ AnyTLS 入站已添加\n\n{msg}")


async def cmd_addhy2(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    args = context.args or []
    if not args:
        await update.effective_message.reply_text(f"添加 Hysteria 2：/addhy2 <tag> [有效期天数]\n{tls_config_hint()}\n随机 UDP 端口、随机密码。", reply_markup=cancel_keyboard())
        return
    try:
        days = parse_duration_days(args[1]) if len(args) > 1 else None
        ok, msg = add_hysteria2_inbound(args[0], days)
    except Exception as exc:
        ok, msg = False, str(exc)
    store.log_op(update.effective_user.id, "addhy2", f"{args[0]} {'OK' if ok else 'FAILED'}")
    await update.effective_message.reply_text(("✅ Hysteria 2 入站已添加\n" if ok else "❌ 添加失败：") + msg)


async def cmd_add(update: Update, context: ContextTypes.DEFAULT_TYPE):
    """/add <tag> <有效期> <流量G> —— 快速添加协议。"""
    if not await guarded(update): return
    args = context.args or []
    if not args:
        await update.effective_message.reply_text(
            "➕ 快速添加协议\n"
            "发送：/add <tag> <有效期> <流量G>\n"
            "例：/add newuser 30 500\n"
            "例：/add newuser permanent 100\n\n"
            "有效期：数字天数 / 1y / 3m / 2w / 7d / 0或permanent（永久）。\n"
            "流量G：该入站月流量上限（GiB），0 表示不限。"
        )
        return
    tag = args[0]
    expires_days = parse_duration_days(args[1]) if len(args) > 1 else None
    limit_gb = None
    if len(args) > 2:
        try:
            limit_gb = float(args[2])
        except ValueError:
            limit_gb = None
        if limit_gb is not None and limit_gb < 0:
            limit_gb = None
    ok, msg = add_vless_reality_inbound(tag, None, expires_days)
    if not ok:
        await update.effective_message.reply_text(f"❌ 添加失败：{msg}")
        return
    if limit_gb is not None:
        store.set_limit(tag, int(limit_gb * 1024 ** 3))
    store.log_op(update.effective_user.id, "add", f"{tag} exp={expires_days} limit={limit_gb}")
    extra = ""
    if limit_gb is not None:
        extra = f"\n月限额：{'不限' if limit_gb == 0 else f'{limit_gb} GiB'}"
    if expires_days is not None:
        extra += f"\n有效期：{expires_days} 天（{expiry_text(tag, store.expiry(tag))}）"
    await update.effective_message.reply_text(f"✅ 已添加入站 {tag}\n\n{msg}{extra}")


async def cmd_protocols(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    page = 0
    if context.args:
        try:
            page = int(context.args[0]) - 1
        except ValueError:
            page = 0
    text, kb = inbound_list_page(page)
    await update.effective_message.reply_text(text, reply_markup=kb)


async def cmd_setexpiry(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    args = context.args or []
    if len(args) < 2:
        await update.effective_message.reply_text(
            "用法：/setexpiry <tag> <有效期>\n"
            "例：/setexpiry newuser 30\n"
            "例：/setexpiry newuser 1y\n"
            "有效期填 0 或 permanent 表示永久。"
        )
        return
    tag = args[0]
    if tag not in inbound_map():
        await update.effective_message.reply_text(f"未找到入站 tag {tag}。")
        return
    raw = " ".join(args[1:])
    try:
        expires_days = parse_duration_days(raw)
    except ValueError as exc:
        await update.effective_message.reply_text(f"❌ {exc}")
        return
    store.set_expiry(tag, expiry_timestamp(expires_days))
    store.log_op(update.effective_user.id, "setexpiry", f"{tag} -> {'永久' if expires_days is None else str(expires_days) + '天'}")
    await update.effective_message.reply_text(f"✅ 已设置 {tag} 有效期：{expiry_text(tag, store.expiry(tag))}")


async def bwh_cmd(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    if not BWH_KEY or not BWH_VEID:
        await update.effective_message.reply_text("请在 .env 中设置 BWH_API_KEY 和 BWH_VEID，之后重新创建容器。"); return
    cmd = (context.args[0] if context.args else "status").lower()
    actor = update.effective_user.id
    async with aiohttp.ClientSession() as session:
        if cmd == "status":
            await update.effective_message.reply_text(await bwh_status_text(session)); return
        elif cmd == "start":
            data = await bwh(session, "start", actor=actor, desc="开机")
            await update.effective_message.reply_text("✅ 开机指令已发送。" if data.get("error") == 0 else f"失败：{data.get('message', data.get('error'))}")
        elif cmd == "stop":
            data = await bwh(session, "stop", actor=actor, desc="强制关机")
            await update.effective_message.reply_text("✅ 强制关机指令已发送。" if data.get("error") == 0 else f"失败：{data.get('message', data.get('error'))}")
        elif cmd == "reboot":
            data = await bwh(session, "reboot", actor=actor, desc="重启")
            await update.effective_message.reply_text("✅ 重启指令已发送。" if data.get("error") == 0 else f"失败：{data.get('message', data.get('error'))}")
        elif cmd == "transfer":
            await update.effective_message.reply_text(await bwh_transfer_text(session)); return
        elif cmd == "resetpwd":
            data = await bwh(session, "resetRootPassword", actor=actor, desc="重置root密码")
            if data.get("error") == 0:
                pwd = data.get("password", "")
                msg = "✅ root 密码已重置。\n新密码：`" + pwd + "`"
                await update.effective_message.reply_text(msg, parse_mode="Markdown")
            else:
                await update.effective_message.reply_text(f"失败：{data.get('message', data.get('error'))}")
        elif cmd == "snap":
            data = await bwh(session, "snapshot/list")
            snaps = data.get("snapshots", [])
            if not snaps:
                await update.effective_message.reply_text("当前没有快照。"); return
            lines = ["📸 快照列表："]
            for s in snaps:
                size = human(int(s.get("size", 0)) * 1024 * 1024) if s.get("size") else "?"
                lines.append(f"• {s.get('fileName', '?')}\n  {s.get('description', '')}｜{size}｜{'📌置顶' if s.get('sticky') else '自动清理'}")
            lines.append("\n用法：/bwh snap_create <描述>\n/bwh snap_restore <fileName>\n/bwh snap_delete <fileName>")
            await update.effective_message.reply_text("\n".join(lines))
        elif cmd == "snap_create":
            desc = " ".join(context.args[1:]) or f"sbbot-{datetime.now(TZ):%Y%m%d-%H%M}"
            data = await bwh(session, "snapshot/create", {"description": desc}, actor=actor, desc="创建快照")
            if data.get("error") == 0:
                await update.effective_message.reply_text(f"✅ 快照已创建：{desc}")
            else:
                await update.effective_message.reply_text(f"失败：{data.get('message', data.get('error'))}")
        elif cmd == "snap_delete":
            if len(context.args) < 2:
                await update.effective_message.reply_text("用法：/bwh snap_delete <fileName>"); return
            fn = context.args[1]
            data = await bwh(session, "snapshot/delete", {"snapshot": fn}, actor=actor, desc=f"删除快照 {fn}")
            await update.effective_message.reply_text("✅ 快照已删除。" if data.get("error") == 0 else f"失败：{data.get('message', data.get('error'))}")
        elif cmd == "snap_restore":
            if len(context.args) < 2:
                await update.effective_message.reply_text("用法：/bwh snap_restore <fileName>（⚠️ 将覆盖服务器全部数据）"); return
            fn = context.args[1]
            data = await bwh(session, "snapshot/restore", {"snapshot": fn}, actor=actor, desc=f"恢复快照 {fn}")
            await update.effective_message.reply_text("✅ 恢复指令已发送，服务器将用该快照覆盖并停机。" if data.get("error") == 0 else f"失败：{data.get('message', data.get('error'))}")
        elif cmd == "logs":
            rows = store.ops(20)
            lines = ["📜 最近操作日志："]
            for ts, who, action, detail in rows:
                lines.append(f"{datetime.fromtimestamp(ts, TZ):%m-%d %H:%M} [{who}] {action} {detail}")
            await update.effective_message.reply_text("\n".join(lines) if len(lines) > 1 else "暂无日志。")
        else:
            await update.effective_message.reply_text("用法：/bwh <status|transfer|start|stop|reboot|resetpwd|snap|snap_create|snap_delete|snap_restore|logs>")


def snapshot_id(filename):
    return hashlib.sha256(filename.encode()).hexdigest()[:24]


async def snapshot_list(session):
    result = await bwh(session, "snapshot/list")
    if result.get("error") != 0:
        raise ValueError(str(result.get("message", result.get("error")))[:500])
    snapshots = result.get("snapshots", [])
    if not isinstance(snapshots, list):
        raise ValueError("快照列表格式错误")
    return [item for item in snapshots if isinstance(item, dict) and item.get("fileName")]


async def snapshot_buttons(message, context, session, sub, actor):
    back = InlineKeyboardMarkup([[InlineKeyboardButton("« 返回快照列表", callback_data="bwh:snap")]])
    try:
        if sub == "auto":
            await message.reply_text("自动备份功能已移除。", reply_markup=back)
            return
        if sub == "snapnew":
            await message.reply_text("⏳ 正在提交创建快照请求…")
            description = f"sbbot-{datetime.now(TZ):%Y%m%d-%H%M%S}"
            result = await bwh(session, "snapshot/create", {"description": description}, actor=actor, desc="手动创建快照")
            text = f"✅ 创建请求已提交：{description}。稍后刷新列表查看。" if result.get("error") == 0 else f"❌ 创建失败：{result.get('message', result.get('error'))}"
            await message.reply_text(text, reply_markup=back)
            return
        snapshots = await snapshot_list(session)
        if sub == "snap" or sub.startswith("snappage:"):
            context.user_data.pop("snapshot_delete_pending", None)
            page = int(sub.partition(":")[2] or 0)
            page = max(0, min(page, max(0, (len(snapshots) - 1) // 8)))
            rows = []
            lines = [f"📸 快照管理（共 {len(snapshots)} 个，第 {page + 1} 页）"]
            for n, item in enumerate(snapshots[page * 8:page * 8 + 8], page * 8 + 1):
                filename = str(item["fileName"])
                lines.append(f"{n}. {filename[:180]}\n   {str(item.get('description', ''))[:120]}")
                rows.append([InlineKeyboardButton(f"📸 选择快照 {n}", callback_data=f"bwh:snapview:{snapshot_id(filename)}"), InlineKeyboardButton(f"♻️ 恢复 {n}", callback_data=f"bwh:snaprestore:{snapshot_id(filename)}")])
            if not snapshots:
                lines.append("当前没有快照，点击下方创建。")
            navigation = []
            if page:
                navigation.append(InlineKeyboardButton("上一页", callback_data=f"bwh:snappage:{page - 1}"))
            if (page + 1) * 8 < len(snapshots):
                navigation.append(InlineKeyboardButton("下一页", callback_data=f"bwh:snappage:{page + 1}"))
            if navigation:
                rows.append(navigation)
            rows.extend([[InlineKeyboardButton("➕ 创建快照", callback_data="bwh:snapnew"), InlineKeyboardButton("🔄 刷新列表", callback_data="bwh:snap")], [InlineKeyboardButton("« 返回面板", callback_data="panel")]])
            await message.reply_text("\n\n".join(lines), reply_markup=InlineKeyboardMarkup(rows))
            return
        action, _, token = sub.partition(":")
        if action == "snapdelete":
            pending = context.user_data.pop("snapshot_delete_pending", None)
            if not pending or pending[0] != token:
                await message.reply_text("删除按钮已失效，请重新选择快照。", reply_markup=back)
                return
            filename = pending[1]
            if not any(str(x["fileName"]) == filename for x in snapshots):
                await message.reply_text("快照已不存在。", reply_markup=back)
                return
            result = await bwh(session, "snapshot/delete", {"snapshot": filename}, actor=actor, desc=f"删除快照 {filename}")
            await message.reply_text("✅ 快照已删除。" if result.get("error") == 0 else "❌ 删除失败，请查看操作日志。", reply_markup=back)
            return
        if action == "snapconfirm":
            pending = context.user_data.pop("snapshot_restore_pending", None)
            if not pending or pending[0] != token:
                await message.reply_text("确认已失效，请重新选择快照。", reply_markup=back)
                return
            filename = pending[1]
        elif action in {"snaprestore", "snapview"}:
            filename = next((str(item["fileName"]) for item in snapshots if snapshot_id(str(item["fileName"])) == token), None)
        else:
            await message.reply_text("旧快照按钮已失效，请重新打开快照列表。", reply_markup=back)
            return
        if not filename or not any(str(item["fileName"]) == filename for item in snapshots):
            await message.reply_text("快照不存在或按钮已失效，请刷新列表。", reply_markup=back)
            return
        if action == "snapview":
            nonce = secrets.token_hex(12)
            context.user_data["snapshot_delete_pending"] = (nonce, filename)
            await message.reply_text(f"已选择快照：{filename}\n点击删除将永久删除此快照。", reply_markup=InlineKeyboardMarkup([
                [InlineKeyboardButton("🗑 删除快照", callback_data=f"bwh:snapdelete:{nonce}")],
                [InlineKeyboardButton("取消", callback_data="bwh:snap")]]))
            return
        if action != "snapconfirm":
            nonce = secrets.token_hex(12)
            context.user_data["snapshot_restore_pending"] = (nonce, filename)
            keyboard = InlineKeyboardMarkup([[InlineKeyboardButton("⚠️ 确认恢复", callback_data=f"bwh:snapconfirm:{nonce}")], [InlineKeyboardButton("取消，返回列表", callback_data="bwh:snap")]])
            await message.reply_text(f"恢复快照：{filename}\n⚠️ 恢复将覆盖目标服务器当前数据并使其停机。确认恢复？", reply_markup=keyboard)
            return
        await message.reply_text("⏳ 正在提交恢复快照请求…")
        result = await bwh(session, "snapshot/restore", {"snapshot": filename}, actor=actor, desc=f"恢复快照 {filename}")
        text = "✅ 恢复指令已发送，目标服务器将停机并用该快照覆盖。" if result.get("error") == 0 else f"❌ 恢复失败：{result.get('message', result.get('error'))}"
        await message.reply_text(text, reply_markup=back)
    except (ValueError, TypeError, KeyError):
        await message.reply_text("❌ 获取或处理快照列表失败，请稍后刷新重试，或检查搬瓦工 API 配置及操作日志。", reply_markup=back)


async def help_cmd(update: Update, context: ContextTypes.DEFAULT_TYPE):
    if not await guarded(update): return
    await update.effective_message.reply_text(
        "/panel - 操作面板\n/today - 今日各入站流量（降序）\n/month - 本月各入站流量（降序）\n"
        "/status - 全部 tag 今日与本月流量\n/usage <tag|端口> - 查询单个 tag\n"
        "/limit <tag|端口> <GiB> - 设置月限额（0 不限）\n/block <tag> /unblock <tag> - 暂停/恢复入站\n"
        "/share <tag1> [tag2...] - 生成 v2rayN 分享链接\n"
        "/addout - 交互式添加出站（SOCKS5/AnyTLS/VLESS Reality/Hysteria 2）\n"
        "/addvless <tag> [伪装域名] [有效期天数] - 添加 VLESS Reality 入站（随机端口/直连/自动放行UFW）\n"
        "/addhy2 <tag> [有效期天数] - 添加 Hysteria 2 入站\n"
        "/addanytls <tag> [有效期天数] - 添加 AnyTLS 入站（证书由 .env 配置）\n"
        "/add <tag> <有效期> <流量G> - 快速添加协议（自动识别 tag/有效期/流量）\n"
        "/protocols - 查看所有入站协议（每页10条）\n"
        "/ufw list|allow|delete - 查询/放行/删除 UFW 端口\n"
        "/bwh <status|transfer|start|stop|reboot|resetpwd|snap|snap_create|snap_delete|snap_restore|logs> - 搬瓦工管理\n"
        "/help - 帮助\n\n默认每入站每月 500G，超出自动暂停并通知；每天 9 点推送昨日流量报告。")


# ---------------- 按钮回调 ----------------

async def on_button(update: Update, context: ContextTypes.DEFAULT_TYPE):
    q = update.callback_query
    if q.from_user.id not in ADMINS:
        await q.answer("无权限。", show_alert=True); return
    await q.answer()
    data = q.data or ""
    actor = q.from_user.id
    if data.startswith("fw:"):
        await context.bot_data["forwarder"].button(update, context)
        return
    async with aiohttp.ClientSession() as session:
        if data == "cancel":
            clear_pending(context)
            await q.edit_message_reply_markup(reply_markup=None)
            await q.message.reply_text("已取消当前操作。", reply_markup=panel_keyboard()); return
        if data == "panel":
            clear_pending(context)
            await q.message.reply_text("🤖 sbbot 操作面板\n点选下面的功能按钮：", reply_markup=panel_keyboard()); return
        if data.startswith("use:"):
            period = data.split(":", 1)[1]
            await q.message.reply_text("\n".join(usage_lines(period))); return
        if data == "limits":
            lines = ["🚦 各入站月限额："]
            for tag, port in inbound_map().items():
                lim = store.limit(tag)
                used = store.usage(tag, month_start(datetime.now(TZ)))
                lines.append(f"{tag}：{human(used)}/{('不限' if lim == 0 else human(lim))}")
            lines.append("\n调整：发送 /limit <tag> <GiB>")
            await q.message.reply_text("\n".join(lines)); return
        if data == "blocks":
            blocked = blocked_tags()
            manual = manual_blocks()
            if not blocked:
                text = "当前没有暂停的入站。"
            else:
                text = "⛔ 已暂停的入站：\n" + "\n".join(f"• {t}{'（手动）' if t in manual else '（超限自动）'}" for t in sorted(blocked))
            text += "\n\n手动暂停：/block <tag>\n解除：/unblock <tag>"
            await q.message.reply_text(text); return
        if data == "logs":
            rows = store.ops(20)
            lines = ["📜 最近操作日志："]
            for ts, who, action, detail in rows:
                lines.append(f"{datetime.fromtimestamp(ts, TZ):%m-%d %H:%M} [{who}] {action} {detail}")
            await q.message.reply_text("\n".join(lines) if len(lines) > 1 else "暂无日志。"); return
        if data == "delete:start":
            context.user_data["delete_selection"] = None
            await q.message.reply_text("选择一个要删除的入站或出站 tag，然后点击保存。删除出站时，原绑定入站会自动改为 direct。", reply_markup=deletion_keyboard(load_config(), None)); return
        if data == "delete:noop":
            return
        if data.startswith("delete:pick:"):
            _, _, kind, tag = data.split(":", 3)
            selected = (kind, tag)
            context.user_data["delete_selection"] = selected
            await q.edit_message_reply_markup(reply_markup=deletion_keyboard(load_config(), selected)); return
        if data == "delete:save":
            selected = context.user_data.get("delete_selection")
            if not selected:
                await q.message.reply_text("❌ 请先选择一个 tag。")
                return
            kind, tag = selected
            doc = load_config()
            original = json.loads(json.dumps(doc))
            try:
                port = delete_tag_from_doc(doc, kind, tag)
            except ValueError as exc:
                await q.message.reply_text(f"❌ {exc}"); return
            write_config(doc)
            ok, msg = restart_singbox()
            if not ok:
                write_config(original)
                restart_singbox()
                await q.message.reply_text(f"❌ 删除失败，配置已回滚：{msg}")
                return
            if kind == "inbound" and port:
                subprocess.run(["nsenter", "-t", "1", "-m", "-p", "--", "/usr/sbin/ufw", "delete", "allow", str(port) + ("/udp" if any(x.get("tag") == tag and x.get("type") == "hysteria2" for x in original.get("inbounds", [])) else "")], capture_output=True, text=True, timeout=30)
            context.user_data.pop("delete_selection", None)
            store.log_op(actor, "delete_tag", f"{kind}:{tag}")
            await q.message.reply_text(f"✅ 已删除 {kind}：{tag}\nsing-box 配置校验与重启通过。", reply_markup=panel_keyboard()); return
        if data == "delete:cancel":
            context.user_data.pop("delete_selection", None)
            await q.message.reply_text("已取消删除。", reply_markup=panel_keyboard()); return
        if data == "out:add":
            context.user_data.pop("fw_keywords", None)
            context.user_data["awaiting_socks_outbound"] = True
            await q.message.reply_text(
                "请粘贴 socks://、socks5://、http://、https://、anytls://、vless://（Reality）、hysteria2:// 或 hy2:// 链接，或发送 SOCKS5 信息：\n\nProxy server: <服务器地址>\nport: <端口>\nusername: <用户名>\npassword: <密码>\n\n发送 /cancel 取消。", reply_markup=cancel_keyboard()
            ); return
        if data == "bind:start":
            context.user_data["bind_selection"] = []
            await q.message.reply_text("先选择一个入站 tag，再选择一个出站 tag，最后保存。", reply_markup=binding_keyboard(load_config(), [])); return
        if data == "bind:noop":
            return
        if data.startswith("bind:pick:"):
            tag = data.split(":", 2)[2]
            doc = load_config()
            selected = list(context.user_data.get("bind_selection", []))
            if tag in selected:
                selected.remove(tag)
            else:
                if len(selected) >= 2:
                    await q.message.reply_text("❌ 最多选择两个 tag；请先取消一个再选。")
                    return
                if selected and selection_kind(doc, selected[0]) == selection_kind(doc, tag):
                    kind = "入站" if selection_kind(doc, tag) == "inbound" else "出站"
                    await q.message.reply_text(f"❌ 不能选择两个{kind} tag；必须一个入站 + 一个出站。")
                    return
                selected.append(tag)
            context.user_data["bind_selection"] = selected
            await q.edit_message_reply_markup(reply_markup=binding_keyboard(doc, selected)); return
        if data == "bind:save":
            selected = list(context.user_data.get("bind_selection", []))
            if len(selected) != 2:
                await q.message.reply_text("❌ 请先选择一个入站 tag 和一个出站 tag。")
                return
            doc = load_config()
            original = json.loads(json.dumps(doc))
            try:
                inbound_tag, outbound_tag = validate_binding_selection(doc, selected[0], selected[1])
                bind_route_in_doc(doc, inbound_tag, outbound_tag)
            except ValueError as exc:
                await q.message.reply_text(f"❌ {exc}"); return
            write_config(doc)
            ok, msg = restart_singbox()
            if not ok:
                write_config(original)
                restart_singbox()
                await q.message.reply_text(f"❌ 保存后 sing-box 校验/重启失败，已回滚：{msg}")
                return
            context.user_data.pop("bind_selection", None)
            store.log_op(actor, "bind_route", f"{inbound_tag}->{outbound_tag}")
            await q.message.reply_text(f"✅ 已绑定：{inbound_tag} → {outbound_tag}", reply_markup=panel_keyboard()); return
        if data == "bind:cancel":
            context.user_data.pop("bind_selection", None)
            await q.message.reply_text("已取消绑定。", reply_markup=panel_keyboard()); return
        if data == "ufw:list":
            lines = ufw_ports()
            kb = InlineKeyboardMarkup([
                [InlineKeyboardButton("➕ 放行端口", callback_data="ufw:help:allow"), InlineKeyboardButton("➖ 删除端口", callback_data="ufw:help:delete")],
                [InlineKeyboardButton("🔄 刷新", callback_data="ufw:list"), InlineKeyboardButton("« 返回面板", callback_data="panel")],
            ])
            await q.message.reply_text("🧱 UFW 已放行端口：\n" + ("\n".join(lines) if lines else "无") + "\n\n添加/删除请发送命令：\n/ufw allow <端口> [tcp|udp|both]\n/ufw delete <端口> [tcp|udp|both]", reply_markup=kb); return
        if data.startswith("ufw:help:"):
            action = data.split(":", 2)[2]
            await q.message.reply_text(f"请发送：\n`/ufw {action} <端口> [tcp|udp|both]`\n例：`/ufw {action} 20000 tcp`", parse_mode="Markdown"); return
        if data == "proto:menu":
            await q.message.reply_text("🔐 协议管理：选择要执行的操作：", reply_markup=proto_menu_keyboard()); return
        if data == "proto:vless":
            await q.message.reply_text(
                "➕ 添加 VLESS Reality 入站\n"
                "发送：/addvless <tag> [伪装域名] [有效期天数]\n"
                "例：/addvless newuser\n"
                "或：/addvless newuser itunes.apple.com 30\n\n"
                "自动分配随机端口、生成密钥，出站直连。\n"
                "不填伪装域名默认 itunes.apple.com；不填有效期默认永久。", reply_markup=cancel_keyboard()
            ); return
        if data == "proto:hy2":
            await q.message.reply_text(f"发送：/addhy2 <tag> [有效期天数]\n{tls_config_hint()}\n随机 UDP 端口、随机密码。", reply_markup=cancel_keyboard()); return
        if data == "proto:anytls":
            await q.message.reply_text(
                "➕ 添加 AnyTLS 入站\n"
                "发送：/addanytls <tag> [有效期天数]\n"
                "例：/addanytls newuser\n"
                "或：/addanytls newuser 30\n\n"
                f"{tls_config_hint()}\n"
                "自动分配随机端口、生成密码，出站直连；不填有效期默认永久。", reply_markup=cancel_keyboard()
            ); return
        if data.startswith("proto:list:"):
            page = int(data.split(":")[2])
            text, kb = inbound_list_page(page)
            await q.message.reply_text(text, reply_markup=kb); return
        if data == "proto:exp":
            await q.message.reply_text(
                "⏳ 设置协议有效期\n"
                "发送：/setexpiry <tag> <有效期>\n"
                "例：/setexpiry newuser 30\n"
                "例：/setexpiry newuser 1y\n\n"
                "有效期填 0 或 permanent 表示永久。\n"
                "有效期支持数字天数，或用 1y / 3m / 2w / 7d。", reply_markup=cancel_keyboard()
            ); return
        if data.startswith("protodel:"):
            tag = data.split(":", 1)[1]
            ok, msg = remove_inbound(tag)
            store.log_op(actor, "del_inbound", f"{tag} {'OK' if ok else 'FAIL'}")
            await q.message.reply_text(f"删除入站 {tag}：{'✅ 成功' if ok else '❌ ' + msg}"); return
        if data == "share":
            rows = []
            mapping = inbound_map()
            items = list(mapping.items())
            for i in range(0, len(items), 2):
                rows.append([InlineKeyboardButton(t, callback_data=f"sharepick:{t}") for t, _ in items[i:i + 2]])
            rows.append([InlineKeyboardButton("✅ 生成选中链接", callback_data="sharedone")])
            rows.append([InlineKeyboardButton("« 返回面板", callback_data="panel")])
            context.user_data["share_tags"] = []
            await q.message.reply_text("🔗 转化链接：点选要转换的入站 tag（可多选），然后点「生成」。\n或者直接发命令：/share <tag1> [tag2 ...]", reply_markup=InlineKeyboardMarkup(rows)); return
        if data.startswith("sharepick:"):
            tag = data.split(":", 1)[1]
            chosen = set(context.user_data.get("share_tags", []))
            if tag in chosen:
                chosen.discard(tag)
            else:
                chosen.add(tag)
            context.user_data["share_tags"] = sorted(chosen)
            rows = []
            items = list(inbound_map().items())
            for i in range(0, len(items), 2):
                row = []
                for t, _ in items[i:i + 2]:
                    label = ("✅ " if t in chosen else "") + t
                    row.append(InlineKeyboardButton(label, callback_data=f"sharepick:{t}"))
                rows.append(row)
            rows.append([InlineKeyboardButton("✅ 生成选中链接", callback_data="sharedone")])
            rows.append([InlineKeyboardButton("« 返回面板", callback_data="panel")])
            try:
                await q.edit_message_reply_markup(reply_markup=InlineKeyboardMarkup(rows))
            except Exception:
                await q.message.edit_reply_markup(reply_markup=InlineKeyboardMarkup(rows))
            return
        if data == "sharedone":
            chosen = context.user_data.get("share_tags", [])
            if not chosen:
                await q.message.reply_text("还没选择任何 tag。"); return
            sub, errors = share_links(chosen)
            if errors:
                await q.message.reply_text("以下 tag 无效或不支持：" + "、".join(errors)); return
            import base64 as _b64
            raw = _b64.b64decode(sub).decode()
            await q.message.reply_text(f"✅ 已生成 {len(chosen)} 条链接。\n\n复制下面任意一行 → v2rayN → 服务器 → 从剪贴板导入批量URL：\n\n`{raw}`", parse_mode="Markdown")
            store.log_op(actor, "share", ",".join(chosen))
            return
        if data.startswith("bwh:"):
            if not BWH_KEY or not BWH_VEID:
                await q.message.reply_text("请在 .env 中设置 BWH_API_KEY 和 BWH_VEID，之后重新创建容器。"); return
            sub = data.split(":", 1)[1]
            if sub == "status":
                await q.message.reply_text(await bwh_status_text(session)); return
            if sub == "transfer":
                await q.message.reply_text(await bwh_transfer_text(session)); return
            if sub == "resetpwd":
                kb = InlineKeyboardMarkup([
                    [InlineKeyboardButton("⚠️ 确认重置", callback_data="bwh:resetpwd:go")],
                    [InlineKeyboardButton("« 取消", callback_data="panel")],
                ])
                await q.message.reply_text("🔑 强制重置服务器 root 密码：\n重置后原密码立即失效，新密码会在下一步发给你。确认？", reply_markup=kb); return
            if sub == "resetpwd:go":
                data2 = await bwh(session, "resetRootPassword", actor=actor, desc="重置root密码")
                if data2.get("error") == 0:
                    pwd = data2.get("password", "")
                    await q.message.reply_text("✅ root 密码已重置。\n新密码：`" + pwd + "`", parse_mode="Markdown")
                else:
                    await q.message.reply_text(f"失败：{data2.get('message', data2.get('error'))}")
                return
            if sub == "power":
                kb = InlineKeyboardMarkup([
                    [InlineKeyboardButton("🔄 重启", callback_data="bwh:do:reboot")],
                    [InlineKeyboardButton("▶️ 开机", callback_data="bwh:do:start"), InlineKeyboardButton("⏹ 强制关机", callback_data="bwh:do:stop")],
                    [InlineKeyboardButton("« 返回面板", callback_data="panel")],
                ])
                await q.message.reply_text("⏻ 电源控制（谨慎操作）：", reply_markup=kb); return
            if sub.startswith("do:"):
                action = sub.split(":", 2)[2]
                mapping = {"reboot": ("reboot", "重启"), "start": ("start", "开机"), "stop": ("stop", "强制关机")}
                if action not in mapping:
                    await q.message.reply_text("未知操作。"); return
                ep, label = mapping[action]
                data2 = await bwh(session, ep, actor=actor, desc=label)
                await q.message.reply_text(f"✅ {label}指令已发送。" if data2.get("error") == 0 else f"失败：{data2.get('message', data2.get('error'))}")
                return
            if sub == "auto" or sub.startswith("snap"):
                await snapshot_buttons(q.message, context, session, sub, actor)
                return
            return
        # 未知
        await q.message.reply_text("未知操作。")


# ---------------- 定时 ----------------

async def daily_report(app):
    now = datetime.now(TZ)
    yesterday = day_start(now) - timedelta(days=1)
    key = yesterday.strftime("%F")
    if store.db.execute("SELECT 1 FROM reports WHERE day=?", (key,)).fetchone(): return
    usage = store.usage_all(yesterday)
    valid = set(inbound_map())
    total = 0; lines = [f"📅 昨日流量报告（{key} 00:00-24:00）"]
    for tag, n in sorted(((t, b) for t, b in usage.items() if t in valid), key=lambda kv: kv[1], reverse=True):
        total += n
        lines.append(f"{tag}：{human(n)}")
    lines.append(f"合计：{human(total)}")
    for admin in ADMINS:
        try: await app.bot.send_message(admin, "\n".join(lines))
        except Exception: logging.exception("daily report to %s failed", admin)
    store.db.execute("INSERT OR IGNORE INTO reports(day) VALUES(?)", (key,)); store.db.commit()


async def background(app):
    hour = int(os.getenv("DAILY_REPORT_HOUR", "9"))
    last_report_key = None
    async with aiohttp.ClientSession() as session:
        while True:
            try:
                await collect_once(session)
                now = datetime.now(TZ)
                if now.hour == hour and last_report_key != now.strftime("%F"):
                    await daily_report(app)
                    last_report_key = now.strftime("%F")
            except Exception:
                logging.exception("collector failed; will retry")
            await asyncio.sleep(POLL_SECONDS)


async def post_init(app):
    ensure_api()
    await app.bot_data["forwarder"].start()
    from telegram import BotCommand, MenuButtonCommands
    try:
        await app.bot.set_my_commands([
            BotCommand("panel", "打开操作面板"),
            BotCommand("bind", "添加转发规则"),
            BotCommand("switch", "切换转发规则设置"),
            BotCommand("cs", "设置重试：分钟 次数"),
            BotCommand("cancel", "取消当前操作"),
            BotCommand("today", "今日流量"),
            BotCommand("month", "本月流量"),
            BotCommand("status", "流量状态"),
            BotCommand("share", "生成分享链接"),
            BotCommand("addout", "添加出站"),
            BotCommand("addvless", "添加VLESS Reality入站"),
            BotCommand("addhy2", "添加Hysteria 2入站"),
            BotCommand("addanytls", "添加AnyTLS入站"),
            BotCommand("add", "快速添加协议"),
            BotCommand("protocols", "查看所有入站"),
            BotCommand("setexpiry", "设置协议有效期"),
            BotCommand("bwh", "搬瓦工管理"),
            BotCommand("help", "帮助"),
        ])
        await app.bot.set_chat_menu_button(menu_button=MenuButtonCommands())
    except Exception:
        logging.exception("set commands failed")
    app.bot_data["background_task"] = asyncio.create_task(background(app))


def main():
    token = os.getenv("TELEGRAM_BOT_TOKEN", "").strip()
    if not token or not ADMINS:
        logging.critical("启动失败：请在 .env 中设置 TELEGRAM_BOT_TOKEN 和 TELEGRAM_ADMIN_IDS；用户 ID 必须是正整数，多个 ID 使用逗号分隔。")
        raise SystemExit(1)
    app = Application.builder().token(token).post_init(post_init).build()
    from forwarder import Forwarder
    fw = Forwarder(app, ADMINS)
    app.bot_data["forwarder"] = fw
    app.add_handler(CommandHandler("bind", fw.bind))
    app.add_handler(CommandHandler("switch", fw.switch))
    app.add_handler(CommandHandler("cs", fw.retry_config))
    app.add_handler(CommandHandler("start", cmd_panel)); app.add_handler(CommandHandler("panel", cmd_panel))
    app.add_handler(CommandHandler("help", help_cmd))
    app.add_handler(CommandHandler("status", status)); app.add_handler(CommandHandler("usage", usage)); app.add_handler(CommandHandler("limit", limit))
    app.add_handler(CommandHandler("today", cmd_today)); app.add_handler(CommandHandler("month", cmd_month))
    app.add_handler(CommandHandler("block", block)); app.add_handler(CommandHandler("unblock", unblock))
    app.add_handler(CommandHandler("share", cmd_share))
    app.add_handler(CommandHandler("addout", cmd_addout))
    app.add_handler(CommandHandler("cancel", cancel_input))
    app.add_handler(CommandHandler("ufw", cmd_ufw))
    app.add_handler(CommandHandler("addvless", cmd_addvless))
    app.add_handler(CommandHandler("addhy2", cmd_addhy2))
    app.add_handler(CommandHandler("addanytls", cmd_addanytls))
    app.add_handler(CommandHandler("add", cmd_add))
    app.add_handler(CommandHandler("protocols", cmd_protocols))
    app.add_handler(CommandHandler("setexpiry", cmd_setexpiry))
    app.add_handler(CommandHandler("bwh", bwh_cmd))
    app.add_handler(CallbackQueryHandler(on_button))
    app.add_handler(MessageHandler(filters.TEXT & ~filters.COMMAND, text_input))
    app.run_polling(drop_pending_updates=False)


if __name__ == "__main__": main()
