"""Persistent Telegram forwarding, isolated from sing-box administration."""
import asyncio
import json
import html
import os
import re
import sqlite3
import time
from pathlib import Path
from urllib.parse import urlsplit
from telegram import InlineKeyboardButton as B, InlineKeyboardMarkup as K
from telethon import TelegramClient, events, utils, errors
from telethon.tl.functions.messages import CheckChatInviteRequest
from urllib.parse import parse_qs


API_CONFIG_HINT = '请在 .env 中设置 Telegram 开发者 API：FORWARD_API_ID（正整数）和 FORWARD_API_HASH，之后重新创建容器。'


def api_configured():
    value = os.getenv('FORWARD_API_ID', '').strip()
    return value.isdigit() and int(value) > 0 and bool(os.getenv('FORWARD_API_HASH', '').strip())


def peer(value):
    value = value.strip()
    if re.fullmatch(r'-?\d+', value):
        return int(value)
    if value.startswith('@') and re.fullmatch(r'@[A-Za-z0-9_]+', value):
        return value
    u = urlsplit(value if '://' in value else 'https://' + value)
    if u.hostname not in {'t.me', 'telegram.me', 'www.t.me'}:
        raise ValueError('请使用 Telegram 链接、@用户名或数字聊天 ID')
    parts = u.path.strip('/').split('/')
    if len(parts) == 2 and parts[0] == 'c' and parts[1].isdigit():
        return int('-100' + parts[1])
    if len(parts) != 1 or not re.fullmatch(r'[A-Za-z0-9_]+', parts[0]):
        raise ValueError('私密群请先加入，再使用数字聊天 ID；规则不能使用单条消息链接')
    return '@' + parts[0]


def accepted(text, mode, words):
    hit = any(w.casefold() in text.casefold() for w in words if w)
    return hit if mode == 'white' else not hit


class Forwarder:
    def __init__(self, app, admins, path='/data/forwarder.db'):
        self.app, self.admins = app, admins
        self.db = sqlite3.connect(path)
        self.db.row_factory = sqlite3.Row
        self.db.execute('PRAGMA journal_mode=WAL')
        self.db.executescript('''
        CREATE TABLE IF NOT EXISTS rules(id INTEGER PRIMARY KEY AUTOINCREMENT, owner INTEGER,
          source INTEGER, target INTEGER, source_label TEXT, target_label TEXT,
          mode TEXT DEFAULT 'poll', filter TEXT DEFAULT 'black', words TEXT DEFAULT '[]',
          cursor INTEGER DEFAULT 0, enabled INTEGER DEFAULT 1);
        CREATE TABLE IF NOT EXISTS logs(id INTEGER PRIMARY KEY, rule INTEGER, ts INTEGER, message TEXT);
        CREATE TABLE IF NOT EXISTS sent(rule INTEGER, mid INTEGER, PRIMARY KEY(rule,mid));
        ''')
        columns = {row[1] for row in self.db.execute('PRAGMA table_info(rules)')}
        for name, default in [('retry_minutes', 5), ('retry_max', 6)]:
            if name not in columns:
                self.db.execute(f'ALTER TABLE rules ADD COLUMN {name} INTEGER DEFAULT {default}')
        self.db.executescript("""
        CREATE TABLE IF NOT EXISTS deliveries(
          rule INTEGER, mid INTEGER, text TEXT, attempts INTEGER DEFAULT 0,
          state TEXT DEFAULT 'waiting', due REAL DEFAULT 0, sent_at REAL,
          PRIMARY KEY(rule,mid));
        CREATE TABLE IF NOT EXISTS receipts(
          target INTEGER, outgoing INTEGER, rule INTEGER, mid INTEGER, attempt INTEGER,
          PRIMARY KEY(target,outgoing));
        """)
        self.db.commit()
        self.client = None
        self.ready = False
        self.status = '连接中'
        self.locks = {}
        self.auth_lock = asyncio.Lock()
        self.login_owner = None
        self.login_until = 0
        self.handler_added = False

    def log(self, rid, message):
        self.db.execute('INSERT INTO logs(rule,ts,message) VALUES(?,?,?)', (rid, int(time.time()), message[:500]))
        self.db.execute('DELETE FROM logs WHERE id NOT IN (SELECT id FROM logs ORDER BY id DESC LIMIT 1000)')
        self.db.commit()

    def rows(self):
        return self.db.execute('SELECT * FROM rules ORDER BY id').fetchall()

    def rule(self, rid, owner):
        row = self.db.execute('SELECT * FROM rules WHERE id=? AND owner=?', (rid, owner)).fetchone()
        if not row:
            raise ValueError('规则不存在或无权操作')
        return row

    async def start(self):
        self.task = asyncio.create_task(self.run())

    async def ensure_client(self):
        if not api_configured():
            raise ValueError(API_CONFIG_HINT)
        if self.client is None:
            self.client = TelegramClient('/data/forwarder-user', int(os.environ['FORWARD_API_ID']), os.environ['FORWARD_API_HASH'], flood_sleep_threshold=0)
        if not self.client.is_connected():
            await self.client.connect()

    async def activate(self):
        if not self.handler_added:
            self.client.add_event_handler(self.event, events.NewMessage())
            self.client.add_event_handler(self.failure_reply, events.MessageEdited())
            self.handler_added = True
        self.ready, self.status = True, '用户会话已连接'

    async def connect(self):
        async with self.auth_lock:
            await self.ensure_client()
            if await self.client.get_me() is None:
                self.ready, self.status = False, '尚未登录，请点击转发助手登录'
                return
            await self.activate()

    async def login_menu(self, message, owner, context):
        if not api_configured():
            await message.reply_text(API_CONFIG_HINT)
            return
        if message.chat.type != 'private':
            await message.reply_text('请在与机器人的私聊中打开转发助手。')
            return
        try:
            async with self.auth_lock:
                await self.ensure_client()
                # get_me performs a live authorization check rather than relying on a cached flag.
                if await self.client.get_me() is not None:
                    await self.activate()
                    context.user_data.pop('fw_login', None)
                    await self.panel(message, owner)
                    return
                self.ready = False
                if self.login_owner not in (None, owner) and self.login_until > time.time():
                    await message.reply_text('另一位管理员正在登录，请稍后重试。')
                    return
                self.login_owner, self.login_until = owner, time.time() + 600
                context.user_data.pop('awaiting_socks_outbound', None)
                context.user_data['fw_login'] = {'step': 'phone', 'expires': self.login_until, 'owner': owner}
                await message.reply_text('尚未登录 Telegram。请输入带国家区号的手机号码，例如 +12025550123。发送 /cancel 取消。')
        except Exception as exc:
            await message.reply_text('无法检查 Telegram 登录状态，请稍后重试：' + type(exc).__name__)

    async def login_input(self, update, context):
        state = context.user_data.get('fw_login')
        if not state:
            return False
        message, owner = update.effective_message, update.effective_user.id
        if owner not in self.admins or message.chat.type != 'private':
            return True
        value = message.text or ''
        try:
            await message.delete()
        except Exception:
            pass
        if state['expires'] < time.time() or self.login_owner != owner:
            context.user_data.pop('fw_login', None)
            await message.reply_text('登录操作已过期，请重新点击转发助手。')
            return True
        async with self.auth_lock:
            try:
                await self.ensure_client()
                if state['step'] == 'phone':
                    phone = value.strip()
                    if not re.fullmatch(r'\+[1-9][0-9]{6,14}', phone):
                        await message.reply_text('手机号格式不正确，例如 +12025550123，请重新输入。')
                        return True
                    sent = await self.client.send_code_request(phone)
                    state.update(step='code', phone=phone, phone_code_hash=sent.phone_code_hash)
                    await message.reply_text('验证码已请求，请查看 Telegram 官方消息或短信。请输入验证码，数字间加空格，例如 1 2 3 4 5，避免验证码直接发送后失效。/cancel 取消。')
                    return True
                if state['step'] == 'code':
                    code = re.sub(r'[\s-]', '', value)
                    if not re.fullmatch(r'[0-9]{4,8}', code):
                        await message.reply_text('请输入验证码数字，数字间加空格，例如 1 2 3 4 5。')
                        return True
                    await self.client.sign_in(phone=state['phone'], code=code, phone_code_hash=state['phone_code_hash'])
                else:
                    await self.client.sign_in(password=value)
                if await self.client.get_me() is None:
                    raise ValueError('登录尚未完成')
                await self.activate()
                context.user_data.pop('fw_login', None)
                self.login_owner, self.login_until = None, 0
                await message.reply_text('✅ Telegram 登录成功。')
                await self.panel(message, owner)
            except errors.SessionPasswordNeededError:
                state['step'] = 'password'
                await message.reply_text('账号启用了两步验证，请输入 Telegram 两步验证密码；输入消息会尝试自动删除。/cancel 取消。')
            except (errors.PhoneCodeInvalidError, errors.PasswordHashInvalidError):
                await message.reply_text('验证码或两步验证密码错误，请重新输入。')
            except errors.PhoneCodeExpiredError:
                state.clear()
                state.update(step='phone', expires=self.login_until, owner=owner)
                await message.reply_text('验证码已过期，请重新输入手机号获取验证码。')
            except errors.PhoneNumberInvalidError:
                await message.reply_text('Telegram 不接受此手机号，请检查国家区号和号码。')
            except errors.FloodWaitError as exc:
                await message.reply_text(f'请求过于频繁，请等待 {exc.seconds} 秒后重试。')
            except Exception as exc:
                await message.reply_text('登录失败，请重试或 /cancel 后重新打开：' + type(exc).__name__)
        return True

    async def list_rules(self, message, owner, page=0):
        rules = [r for r in self.rows() if r['owner'] == owner]
        page = max(0, min(page, (len(rules)-1)//20))
        rows = [[B(f"ID{r['id']}：{r['source_label']} → {r['target_label']}"[:100], callback_data=f"fw:rule:{r['id']}")] for r in rules[page*20:(page+1)*20]]
        nav = []
        if page > 0: nav.append(B('上一页', callback_data=f'fw:list:{page-1}'))
        if (page+1)*20 < len(rules): nav.append(B('下一页', callback_data=f'fw:list:{page+1}'))
        if nav: rows.append(nav)
        rows += [[B('➕ 添加转发规则', callback_data='fw:add')], [B('« 返回转发助手', callback_data='fw:menu')]]
        await message.reply_text(f'请选择要设置的规则（共 {len(rules)} 条）：' if rules else '暂无转发规则，请先添加。', reply_markup=K(rows))

    async def run(self):
        if not api_configured():
            self.status = API_CONFIG_HINT
            return
        while True:
            try:
                if not self.ready:
                    await self.connect()
                if not self.ready:
                    await asyncio.sleep(30)
                    continue
                for r in self.rows():
                    if r['enabled']:
                        # Realtime also reconciles missed updates after reconnect/restart.
                        await self.drain(r['id'])
                await self.retry_due()
                await asyncio.sleep(30)
            except asyncio.CancelledError:
                if self.client:
                    await self.client.disconnect()
                raise
            except Exception as exc:
                self.status = '连接/读取异常：' + type(exc).__name__
                self.log(0, self.status)
                await asyncio.sleep(30)

    async def event(self, event):
        await self.failure_reply(event)
        for r in self.rows():
            if r['enabled'] and r['mode'] == 'live' and r['source'] == event.chat_id:
                await self.drain(r['id'])

    async def drain(self, rid):
        async with self.locks.setdefault(rid, asyncio.Lock()):
            r = self.db.execute('SELECT * FROM rules WHERE id=?', (rid,)).fetchone()
            if not r or not r['enabled']:
                return
            try:
                async for msg in self.client.iter_messages(r['source'], min_id=r['cursor'], reverse=True, limit=100):
                    await self.deliver(r, msg)
                    self.db.execute('UPDATE rules SET cursor=? WHERE id=?', (msg.id, rid))
                    self.db.execute('DELETE FROM sent WHERE rule=? AND mid<=?', (rid, msg.id))
                    self.db.commit()
            except Exception as exc:
                if isinstance(exc, errors.UnauthorizedError):
                    self.ready, self.status = False, '用户会话已失效，请重新打开转发助手登录'
                self.log(rid, '转发失败，将重试：' + type(exc).__name__)

    async def expanded(self, msg, depth=0, visited=None):
        visited = set() if visited is None else visited
        key = (msg.chat_id, msg.id)
        if key in visited or depth >= 3:
            return ''
        visited.add(key)
        texts = [msg.raw_text or '']
        for row in (msg.buttons or []):
            for button in row:
                url = getattr(button, 'url', None)
                if url:
                    u = urlsplit(url)
                    m = re.fullmatch(r'/(?:s/)?([A-Za-z0-9_]+)/(\d+)', u.path)
                    private = re.fullmatch(r'/c/(\d+)/(\d+)', u.path)
                    if u.hostname in {'t.me', 'telegram.me'} and (m or private):
                        target = int('-100' + private[1]) if private else '@' + m[1]
                        mid = int((private or m)[2])
                        linked = await self.client.get_messages(target, ids=mid)
                        if linked:
                            texts.append(await self.expanded(linked, depth + 1, visited))
                    else:
                        texts.append('[按钮链接] ' + url)
                elif getattr(button, 'data', None):
                    # Only content-reveal callbacks; never arbitrary purchase/delete buttons.
                    if re.search('查看|展开|全文|内容|详情|阅读|read|more|view', button.text, re.I):
                        result = await asyncio.wait_for(button.click(), 15)
                        if getattr(result, 'message', None):
                            texts.append(result.message)
                        await asyncio.sleep(1)
                        fresh = await self.client.get_messages(msg.chat_id, ids=msg.id)
                        if fresh and fresh.raw_text != msg.raw_text:
                            texts.append(await self.expanded(fresh, depth + 1, visited - {key}))
                        if getattr(result, 'url', None):
                            texts.append('[按钮返回链接] ' + result.url)
        return '\n\n'.join(dict.fromkeys(t for t in texts if t))

    async def deliver(self, r, msg):
        if getattr(msg, 'action', None):
            return
        text = await self.expanded(msg)
        if not accepted(text, r['filter'], json.loads(r['words'])):
            self.log(r['id'], f'消息 {msg.id}：关键词过滤')
            return
        if self.db.execute('SELECT 1 FROM sent WHERE rule=? AND mid=?', (r['id'], msg.id)).fetchone():
            return
        me = await self.app.bot.get_me()
        if r['target'] == me.id:
            # Deliver in the owner's own bot conversation. No media downloads required.
            if msg.media:
                await self.app.bot.forward_message(r['owner'], r['source'], msg.id)
                self.log(r['id'], f'消息 {msg.id}：媒体已投递至机器人')
            for offset in range(0, len(text), 4000):
                await self.app.bot.send_message(r['owner'], text[offset:offset + 4000])
        else:
            outgoing = await self.client.forward_messages(r['target'], msg)
            self.track(r, msg.id, text, outgoing)
            if text != (msg.raw_text or ''):
                for offset in range(0, len(text), 4000):
                    outgoing = await self.client.send_message(r['target'], text[offset:offset + 4000], parse_mode=None)
                    self.track(r, msg.id, text, outgoing)
        self.db.execute('INSERT OR IGNORE INTO sent VALUES(?,?)', (r['id'], msg.id))
        self.db.commit()
        self.log(r['id'], f'消息 {msg.id}：转发成功')

    def track(self, r, mid, text, outgoing):
        self.db.execute('INSERT OR IGNORE INTO deliveries(rule,mid,text,sent_at) VALUES(?,?,?,?)',
                        (r['id'], mid, text, time.time()))
        attempt = self.db.execute('SELECT attempts FROM deliveries WHERE rule=? AND mid=?', (r['id'], mid)).fetchone()[0]
        for sent in outgoing if isinstance(outgoing, list) else [outgoing]:
            oid = getattr(sent, 'id', None)
            if isinstance(oid, int):
                self.db.execute('INSERT OR REPLACE INTO receipts VALUES(?,?,?,?,?)', (r['target'], oid, r['id'], mid, attempt))
        self.db.commit()

    async def failure_reply(self, event):
        if getattr(event, 'out', False) or getattr(event, 'sender_id', None) != event.chat_id:
            return
        if '分享生成STRM文件失败' not in re.sub(r'\s+', '', event.raw_text or ''):
            return
        reply = getattr(event.message, 'reply_to_msg_id', None)
        if reply:
            candidates = self.db.execute("""SELECT d.* FROM deliveries d JOIN receipts x
              ON x.rule=d.rule AND x.mid=d.mid AND x.attempt=d.attempts
              JOIN rules r ON r.id=d.rule WHERE x.target=? AND x.outgoing=? AND d.state IN ('waiting','queued')""",
              (event.chat_id, reply)).fetchall()
        else:
            # Never guess between multiple outstanding messages.
            candidates = self.db.execute("""SELECT d.* FROM deliveries d JOIN rules r ON r.id=d.rule
              WHERE r.target=? AND d.state='waiting' AND d.sent_at>=?""", (event.chat_id, time.time()-86400)).fetchall()
        if len(candidates) != 1:
            self.log(0, 'STRM 失败回复无法唯一关联，请检查目标机器人是否引用原消息')
            return
        d = candidates[0]
        async with self.locks.setdefault(d['rule'], asyncio.Lock()):
            r = self.db.execute('SELECT * FROM rules WHERE id=?', (d['rule'],)).fetchone()
            if not r: return
            current = self.db.execute('SELECT state,attempts FROM deliveries WHERE rule=? AND mid=?', (d['rule'],d['mid'])).fetchone()
            if not current or current['state'] != 'waiting' or current['attempts'] != d['attempts']: return
            state = 'queued' if d['attempts'] < r['retry_max'] else 'exhausted'
            self.db.execute("UPDATE deliveries SET state=?,due=? WHERE rule=? AND mid=? AND state='waiting' AND attempts=?",
                            (state, time.time()+r['retry_minutes']*60, d['rule'], d['mid'], d['attempts']))
            self.db.commit()
            self.log(d['rule'], f"消息 {d['mid']}：STRM 失败，" + ('加入重试队列' if state == 'queued' else '已达到重试上限'))

    async def retry_due(self):
        pending = self.db.execute("SELECT rule,mid FROM deliveries WHERE state='queued' AND due<=?", (time.time(),)).fetchall()
        for item in pending:
            rid, mid = item
            async with self.locks.setdefault(rid, asyncio.Lock()):
                r = self.db.execute('SELECT * FROM rules WHERE id=?', (rid,)).fetchone()
                d = self.db.execute('SELECT * FROM deliveries WHERE rule=? AND mid=?', (rid,mid)).fetchone()
                if not r or not r['enabled'] or not d or d['state'] != 'queued': continue
                if d['attempts'] >= r['retry_max']:
                    self.db.execute("UPDATE deliveries SET state='exhausted' WHERE rule=? AND mid=?", (rid,mid))
                    self.db.commit()
                    continue
                # Persist the attempt before sending: restarts cannot reset the maximum.
                self.db.execute("UPDATE deliveries SET attempts=attempts+1, due=?,sent_at=? WHERE rule=? AND mid=?",
                                (time.time()+r['retry_minutes']*60,time.time(),rid,mid))
                self.db.commit()
                try:
                    msg = await self.client.get_messages(r['source'], ids=mid)
                    if not msg: raise ValueError('source message deleted')
                    outgoing = await self.client.forward_messages(r['target'], msg)
                    self.track(r,mid,d['text'],outgoing)
                    if d['text'] != (getattr(msg, 'raw_text', '') or ''):
                        for offset in range(0,len(d['text']),4000):
                            outgoing = await self.client.send_message(r['target'],d['text'][offset:offset+4000],parse_mode=None)
                            self.track(r,mid,d['text'],outgoing)
                    self.db.execute("UPDATE deliveries SET state='waiting' WHERE rule=? AND mid=?", (rid,mid))
                    self.db.commit()
                    self.log(rid, f"消息 {mid}：第 {d['attempts']+1} 次重试已发送")
                except Exception as exc:
                    if d['attempts']+1 >= r['retry_max']:
                        self.db.execute("UPDATE deliveries SET state='exhausted' WHERE rule=? AND mid=?", (rid,mid))
                    if isinstance(exc, errors.FloodWaitError):
                        self.db.execute('UPDATE deliveries SET due=? WHERE rule=? AND mid=?',
                                        (time.time()+max(exc.seconds,r['retry_minutes']*60),rid,mid))
                    self.db.commit()
                    self.log(rid, f'消息 {mid}：重试发送失败：' + type(exc).__name__)

    async def retry_page(self, message, owner, rid, page=0):
        r = self.rule(rid, owner)
        items = self.db.execute("SELECT * FROM deliveries WHERE rule=? AND state='queued' ORDER BY due,mid", (rid,)).fetchall()
        pages = max(1,(len(items)+9)//10)
        page = max(0,min(page,pages-1))
        lines = [f'转发重试 · ID{rid} · {page+1}/{pages} 页 · 待重试 {len(items)} 条']
        for d in items[page*10:(page+1)*10]:
            source = str(r['source'])
            link = f"https://t.me/c/{source[4:]}/{d['mid']}" if source.startswith('-100') else ''
            title = html.escape((d['text'] or '（无文本消息，图片不展示）')[:180])
            label = f'<a href="{link}">{title}</a>' if link else title
            lines.append(f"{label}\n已重试 {d['attempts']}/{r['retry_max']}，约 {max(0,int(d['due']-time.time()))} 秒后重试")
        if not items: lines.append('暂无待重试消息。')
        nav = []
        if page: nav.append(B('上一页',callback_data=f'fw:retry:{rid}:{page-1}'))
        if page+1 < pages: nav.append(B('下一页',callback_data=f'fw:retry:{rid}:{page+1}'))
        rows = [nav] if nav else []
        rows += [[B('刷新',callback_data=f'fw:retry:{rid}:{page}')], [B('返回规则',callback_data=f'fw:rule:{rid}')]]
        await message.reply_text('\n\n'.join(lines),parse_mode='HTML',disable_web_page_preview=True,reply_markup=K(rows))

    async def retry_config(self, update, context):
        owner = update.effective_user.id
        if owner not in self.admins: return
        try:
            rid = context.user_data.get('fw_rule')
            if rid is None: raise ValueError('请先打开要修改的转发规则。')
            self.rule(rid,owner)
            if len(context.args) != 2: raise ValueError('用法：/cs 时间 次数，例如 /cs 5 6；时间单位为分钟。')
            minutes, maximum = map(int,context.args)
            if not 1 <= minutes <= 10080 or not 0 <= maximum <= 100: raise ValueError('时间为 1–10080 分钟，次数为 0–100（0 表示关闭重试）。')
            with self.db:
                self.db.execute('UPDATE rules SET retry_minutes=?,retry_max=? WHERE id=?',(minutes,maximum,rid))
                self.db.execute("UPDATE deliveries SET due=?,state=CASE WHEN attempts>=? THEN 'exhausted' ELSE 'queued' END WHERE rule=? AND state='queued'",(time.time()+minutes*60,maximum,rid))
            await update.effective_message.reply_text(f'✅ ID{rid} 重试冷却 {minutes} 分钟，最多 {maximum} 次；待重试消息从现在重新计时。')
        except (ValueError,TypeError) as exc:
            await update.effective_message.reply_text(str(exc))

    async def panel(self, message, owner, rid=None):
        if rid is None:
            lines = ['📨 Telegram频道转发助手', self.status,
                     '频道/群请将自己的 TG 机器人设为管理员；用户会话也必须能读取来源并向目标发送消息。',
                     '新规则从创建后的消息开始，每 30 秒轮询；实时模式即时监听并补取遗漏。',
                     '目标填 bot 可投递到自己的机器人。']
            lines += [f"ID{r['id']}：{r['source_label']} → {r['target_label']}" for r in self.rows() if r['owner'] == owner]
            rows = [[B('➕ 添加转发规则', callback_data='fw:add')], [B('⚙️ 切换至转发规则设置', callback_data='fw:switch')], [B('« 返回面板', callback_data='panel')]]
        else:
            r = self.rule(rid, owner)
            lines = [f"⚙️ ID{rid}：{r['source_label']} → {r['target_label']}", '按钮跳转支持可访问的 TG 消息和查看内容回调；外部网页保留链接。']
            rows = [[B('监控：' + ('轮询 → 点击实时' if r['mode'] == 'poll' else '实时 → 点击轮询'), callback_data=f'fw:mode:{rid}')],
                    [B('过滤：' + ('黑名单 → 点击白名单' if r['filter'] == 'black' else '白名单 → 点击黑名单'), callback_data=f'fw:filter:{rid}')],
                    [B('查看关键词', callback_data=f'fw:words:{rid}'), B('设置关键词', callback_data=f'fw:edit:{rid}')],
                    [B('日志（最近十条）', callback_data=f'fw:logs:{rid}'), B('清除日志', callback_data=f'fw:clear:{rid}')],
                    [B('🔁 转发重试', callback_data=f'fw:retry:{rid}:0')],
                    [B(f"重试冷却时间：{r['retry_minutes']} 分钟", callback_data=f'fw:retrycfg:{rid}'), B(f"最多次数：{r['retry_max']}", callback_data=f'fw:retrycfg:{rid}')],
                    [B('暂停' if r['enabled'] else '启用', callback_data=f'fw:toggle:{rid}')],
                    [B('🗑 删除规则', callback_data=f'fw:delete:{rid}')],
                    [B('« 返回规则列表', callback_data='fw:switch')],
                    [B('« 返回转发助手', callback_data='fw:menu')]]
        await message.reply_text('\n'.join(lines)[:4000], reply_markup=K(rows))

    async def resolve(self, value):
        u = urlsplit(value if '://' in value else 'https://' + value)
        match = re.fullmatch(r'/(?:\+|joinchat/)([A-Za-z0-9_-]+)', u.path)
        if u.hostname in {'t.me', 'telegram.me'} and match:
            invite = await self.client(CheckChatInviteRequest(match[1]))
            if not getattr(invite, 'chat', None):
                raise ValueError('请先使用配置的 Telegram 用户账号加入该私密群/频道，再添加规则')
            return invite.chat
        return await self.client.get_entity(peer(value))

    async def bind(self, update, context):
        owner = update.effective_user.id
        if owner not in self.admins: return
        if not api_configured():
            await (update.callback_query.message if update.callback_query else update.effective_message).reply_text(API_CONFIG_HINT)
            return
        try:
            if len(context.args) != 2:
                raise ValueError('用法：/bind 源频道/群/用户链接 目标频道/群/用户链接\n目标可填 bot，表示自己的机器人。频道/群请添加机器人为管理员。')
            if not self.ready: raise ValueError('用户会话未连接：' + self.status)
            source = await self.resolve(context.args[0])
            if getattr(source, "left", False):
                raise ValueError("请先使用配置的 Telegram 用户账号加入来源频道/群，确保实时监听有效")
            target_arg = context.args[1]
            target = await self.resolve('@' + (await self.app.bot.get_me()).username if target_arg == 'bot' else target_arg)
            sid, tid = utils.get_peer_id(source), utils.get_peer_id(target)
            if sid == tid: raise ValueError('来源和目标不能相同')
            if sid == (await self.app.bot.get_me()).id: raise ValueError('不能监控本机器人，避免控制消息形成循环')
            # Reject cycles across all owners, including paths through disabled rules.
            graph = {}
            for r in self.rows(): graph.setdefault(r['source'], []).append(r['target'])
            todo, seen = [tid], set()
            while todo:
                node = todo.pop()
                if node == sid: raise ValueError('该规则会形成转发循环')
                if node not in seen:
                    seen.add(node); todo.extend(graph.get(node, []))
            if self.db.execute('SELECT 1 FROM rules WHERE owner=? AND source=? AND target=?', (owner,sid,tid)).fetchone():
                raise ValueError('相同规则已存在')
            last = await self.client.get_messages(source, limit=1)
            cur = self.db.execute('INSERT INTO rules(owner,source,target,source_label,target_label,cursor) VALUES(?,?,?,?,?,?)', (owner,sid,tid,context.args[0],target_arg,last[0].id if last else 0))
            self.db.commit()
            await self.panel(update.effective_message, owner, cur.lastrowid)
        except ValueError as exc:
            await update.effective_message.reply_text(str(exc))
        except Exception as exc:
            await update.effective_message.reply_text('无法解析/访问聊天，请确认用户会话的权限：' + type(exc).__name__)

    async def switch(self, update, context):
        if update.effective_user.id not in self.admins: return
        if not api_configured():
            await update.effective_message.reply_text(API_CONFIG_HINT)
            return
        if not context.args:
            await self.list_rules(update.effective_message, update.effective_user.id)
            return
        try:
            rid = int(context.args[0].removeprefix('ID'))
            self.rule(rid, update.effective_user.id)
            context.user_data['fw_rule'] = rid
            await self.panel(update.effective_message, update.effective_user.id, rid)
        except (ValueError, IndexError):
            await update.effective_message.reply_text('请发送 /switch 1 切换至 ID1（规则必须存在且属于你）。')

    async def button(self, update, context):
        q = update.callback_query
        owner = q.from_user.id
        if owner not in self.admins: return
        if not api_configured():
            await (update.callback_query.message if update.callback_query else update.effective_message).reply_text(API_CONFIG_HINT)
            return
        parts = q.data.split(':'); action = parts[1]
        if action == 'menu':
            context.user_data.pop('fw_keywords', None)
            await self.login_menu(q.message, owner, context); return
        if action in {'switch', 'list'}:
            context.user_data.pop('fw_keywords', None)
            await self.list_rules(q.message, owner, int(parts[2]) if action == 'list' and parts[2].isdigit() else 0); return
        if action == 'add':
            await q.message.reply_text('发送 /bind 源链接 目标链接\n目标可填 bot。频道/群需添加机器人为管理员，用户会话需有访问权限。' if action == 'add' else '发送 /switch 1 切换至 ID1'); return
        try:
            rid = int(parts[2]); r = self.rule(rid, owner)
            context.user_data['fw_rule'] = rid
            if action == 'retry':
                await self.retry_page(q.message,owner,rid,int(parts[3]) if len(parts)>3 else 0); return
            if action == 'retrycfg':
                await q.message.reply_text(f"修改 ID{rid}：发送 /cs 时间 次数\n时间单位：分钟。例如 /cs 5 6（冷却5分钟，最多重试6次）。"); return
            if action == 'delete':
                await q.message.reply_text(f'确定删除规则 ID{rid}？将停止该规则转发并删除它的关键词、日志和去重记录。', reply_markup=K([[B('确认删除', callback_data=f'fw:confirm_delete:{rid}'), B('取消', callback_data=f'fw:rule:{rid}')]])); return
            if action == 'confirm_delete':
                async with self.locks.setdefault(rid, asyncio.Lock()):
                    self.rule(rid, owner)
                    with self.db:
                        self.db.execute('DELETE FROM logs WHERE rule=?', (rid,))
                        self.db.execute('DELETE FROM sent WHERE rule=?', (rid,))
                        self.db.execute('DELETE FROM deliveries WHERE rule=?', (rid,))
                        self.db.execute('DELETE FROM receipts WHERE rule=?', (rid,))
                        self.db.execute('DELETE FROM rules WHERE id=? AND owner=?', (rid, owner))
                if context.user_data.get('fw_keywords') == rid: context.user_data.pop('fw_keywords', None)
                await q.message.reply_text(f'✅ 规则 ID{rid} 已删除。')
                await self.list_rules(q.message, owner); return
            if action in {'mode', 'filter', 'toggle'}:
                field = {'mode':'mode', 'filter':'filter', 'toggle':'enabled'}[action]
                value = ('live' if r['mode']=='poll' else 'poll') if action=='mode' else ('white' if r['filter']=='black' else 'black') if action=='filter' else 1-r['enabled']
                self.db.execute(f'UPDATE rules SET {field}=? WHERE id=?', (value,rid)); self.db.commit()
            elif action == 'words':
                await q.message.reply_text('关键词（包含匹配，不区分大小写）：\n' + ('\n'.join(json.loads(r['words'])) or '（空）') + '\n空黑名单全部转发；空白名单全部过滤。'); return
            elif action == 'edit':
                context.user_data.pop('awaiting_socks_outbound', None)
                context.user_data['fw_keywords'] = rid
                await q.message.reply_text('发送关键词，每行一个（最多100个，每个100字）。发送 - 清空，/cancel 取消。'); return
            elif action == 'logs':
                logs = self.db.execute('SELECT ts,message FROM logs WHERE rule=? ORDER BY id DESC LIMIT 10', (rid,)).fetchall()
                await q.message.reply_text('\n'.join(time.strftime('%m-%d %H:%M:%S',time.localtime(x[0]))+' '+x[1] for x in logs) or '暂无日志'); return
            elif action == 'clear':
                self.db.execute('DELETE FROM logs WHERE rule=?', (rid,)); self.db.commit()
                await q.message.reply_text('✅ 此规则服务器日志已清除。'); return
            await self.panel(q.message, owner, rid)
        except (ValueError, IndexError):
            await q.message.reply_text('规则不存在或按钮失效，请重新打开转发助手。')

    async def keyword_input(self, update, context):
        rid = context.user_data.get('fw_keywords')
        if rid is None: return False
        try:
            self.rule(rid, update.effective_user.id)
        except ValueError:
            context.user_data.pop('fw_keywords', None)
            await update.effective_message.reply_text('规则已删除或无权操作，请重新打开规则列表。')
            return True
        words = [] if update.effective_message.text.strip() == '-' else list(dict.fromkeys(x.strip() for x in update.effective_message.text.splitlines() if x.strip()))
        if len(words)>100 or any(len(x)>100 for x in words):
            await update.effective_message.reply_text('最多100个关键词，每个不超过100字，请重新输入。'); return True
        self.db.execute('UPDATE rules SET words=? WHERE id=?', (json.dumps(words,ensure_ascii=False),rid));self.db.commit()
        context.user_data.pop('fw_keywords',None)
        await update.effective_message.reply_text('✅ 关键词已保存。')
        await self.panel(update.effective_message, update.effective_user.id, rid)
        return True
