import unittest
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch
from forwarder import Forwarder

class RetryTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.f = Forwarder(NS(bot=NS(get_me=AsyncMock(return_value=NS(id=99)))), {1}, ':memory:')
        self.f.db.execute("INSERT INTO rules(owner,source,target,source_label,target_label) VALUES(1,-100123,222,'src','dst')")
        self.f.db.commit()
        self.f.client = NS(forward_messages=AsyncMock(return_value=NS(id=101)),send_message=AsyncMock(return_value=NS(id=103)),get_messages=AsyncMock(return_value=NS(id=7,raw_text="链接 https://example.org <test>")))
        self.f.track(self.f.rule(1,1),7,'链接 https://example.org <test>',NS(id=100))
    def tearDown(self): self.f.db.close()
    def row(self): return self.f.db.execute('SELECT * FROM deliveries WHERE mid=7').fetchone()
    async def emit_failure(self, reply=100):
        await self.f.failure_reply(NS(out=False,sender_id=222,chat_id=222,raw_text='分享生成 STRM 文件失败\n',message=NS(reply_to_msg_id=reply)))
    async def test_cooldown_duplicate_and_six_attempts(self):
        with patch('forwarder.time.time',return_value=1000):
            await self.emit_failure(); await self.emit_failure()
            self.assertEqual(self.row()['due'],1300)
            await self.f.retry_due()
            self.f.client.forward_messages.assert_not_awaited()
        for i in range(6):
            self.f.db.execute('UPDATE deliveries SET due=0'); self.f.db.commit()
            await self.f.retry_due()
            self.assertEqual(self.row()['attempts'],i+1)
            await self.emit_failure(101)
        self.assertEqual(self.row()['state'],'exhausted')
        await self.f.retry_due()
        self.assertEqual(self.f.client.forward_messages.await_count,6)
    async def test_pause_and_send_error(self):
        await self.emit_failure()
        self.f.db.execute('UPDATE deliveries SET due=0')
        self.f.db.execute('UPDATE rules SET enabled=0')
        await self.f.retry_due(); self.f.client.forward_messages.assert_not_awaited()
        self.f.db.execute('UPDATE rules SET enabled=1,retry_max=1')
        self.f.client.forward_messages.side_effect=RuntimeError()
        await self.f.retry_due()
        self.assertEqual(self.row()['state'],'exhausted')
    async def test_ambiguous_unquoted_and_old_reply(self):
        self.f.track(self.f.rule(1,1),8,'other',NS(id=102))
        await self.emit_failure(None)
        self.assertEqual(self.row()['state'],'waiting')
        await self.emit_failure()
        self.f.db.execute('UPDATE deliveries SET due=0')
        await self.f.retry_due()
        await self.emit_failure(100)
        self.assertEqual(self.row()['state'],'waiting')
    async def test_page_configuration_permissions(self):
        await self.emit_failure()
        for mid in range(8,19):
            self.f.track(self.f.rule(1,1),mid,'<text>',NS(id=mid+100))
        self.f.db.execute("UPDATE deliveries SET state='queued',due=0")
        msg=NS(reply_text=AsyncMock())
        await self.f.retry_page(msg,1,1)
        result=msg.reply_text.call_args
        self.assertEqual(result.args[0].count('已重试'),10)
        self.assertIn('&lt;test&gt;',result.args[0])
        self.assertTrue(result.kwargs['disable_web_page_preview'])
        await self.f.retry_page(msg,1,1,1)
        self.assertEqual(msg.reply_text.call_args.args[0].count('已重试'),2)
        with self.assertRaises(ValueError): await self.f.retry_page(msg,2,1)
        ctx=NS(args=['2','3'],user_data={'fw_rule':1})
        await self.f.retry_config(NS(effective_user=NS(id=1),effective_message=msg),ctx)
        self.assertEqual(self.f.rule(1,1)['retry_minutes'],2)
        ctx.args=['0','-1']
        await self.f.retry_config(NS(effective_user=NS(id=1),effective_message=msg),ctx)
        self.assertEqual(self.f.rule(1,1)['retry_max'],3)
