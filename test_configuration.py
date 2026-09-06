import os
import subprocess
import sys
import unittest
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch
import app
from forwarder import Forwarder

class ConfigurationTests(unittest.IsolatedAsyncioTestCase):
    def test_required_startup(self):
        for token, ids in [('', '123'), ('123:fake', ''), ('123:fake', 'invalid'), ('123:fake', '-1')]:
            env = {**os.environ, 'TELEGRAM_BOT_TOKEN': token, 'TELEGRAM_ADMIN_IDS': ids}
            r = subprocess.run([sys.executable, 'app.py'], env=env, capture_output=True, text=True, timeout=15)
            self.assertEqual(r.returncode, 1)
            self.assertIn('启动失败', r.stderr)
            self.assertIn('.env', r.stderr)
            self.assertNotIn('123:fake', r.stderr)

    async def test_tls_command_and_buttons_follow_configuration(self):
        msg = NS(reply_text=AsyncMock())
        q = NS(from_user=NS(id=123), message=msg, answer=AsyncMock(), data='')
        update = NS(effective_user=NS(id=123), effective_message=msg, callback_query=q)
        ctx = NS(args=[], user_data={}, bot_data={})
        with patch.object(app, 'ADMINS', {123}), patch.object(app, 'SHARE_DOMAIN', 'custom.example.org'), patch.object(app, 'ANYTLS_CERT_PATH', '/custom/full.pem'), patch.object(app, 'ANYTLS_KEY_PATH', '/custom/key.pem'), patch.object(app, 'guarded', AsyncMock(return_value=True)):
            for handler in [app.cmd_addanytls, app.cmd_addhy2]:
                await handler(update, ctx)
                self.check_hint(msg.reply_text.call_args.args[0])
            for action in ['proto:anytls', 'proto:hy2']:
                q.data = action
                await app.on_button(update, ctx)
                self.check_hint(msg.reply_text.call_args.args[0])

    def check_hint(self, text):
        for expected in ['custom.example.org', '/custom/full.pem', '/custom/key.pem', '.env']:
            self.assertIn(expected, text)

    async def test_missing_optional_api_buttons(self):
        msg = NS(reply_text=AsyncMock())
        q = NS(from_user=NS(id=123), message=msg, answer=AsyncMock(), data='bwh:status')
        update = NS(effective_user=NS(id=123), effective_message=msg, callback_query=q)
        ctx = NS(args=[], user_data={}, bot_data={})
        with patch.object(app, 'ADMINS', {123}), patch.object(app, 'BWH_KEY', ''), patch.object(app, 'BWH_VEID', ''), patch.object(app, 'bwh', AsyncMock()) as remote:
            await app.on_button(update, ctx)
            self.assertIn('.env', msg.reply_text.call_args.args[0])
            remote.assert_not_awaited()
        with patch.dict(os.environ, {'FORWARD_API_ID': '', 'FORWARD_API_HASH': ''}):
            f = Forwarder(NS(), {123}, ':memory:')
            try:
                q.data = 'fw:menu'
                await f.button(update, ctx)
                self.assertIn('FORWARD_API_ID', msg.reply_text.call_args.args[0])
                self.assertIn('.env', msg.reply_text.call_args.args[0])
                self.assertIsNone(f.client)
            finally:
                f.db.close()
