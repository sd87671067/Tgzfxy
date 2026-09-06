import tempfile
import unittest
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, patch
import app
from forwarder import Forwarder, accepted, peer

class ForwardTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self):
        self.env_patch = patch.dict('os.environ', {'FORWARD_API_ID': '12345', 'FORWARD_API_HASH': 'example-hash'})
        self.env_patch.start()
        self.addCleanup(self.env_patch.stop)
        self.tmp = tempfile.TemporaryDirectory()
        self.bot = NS(get_me=AsyncMock(return_value=NS(id=99,username='mybot')), send_message=AsyncMock(), forward_message=AsyncMock())
        self.f = Forwarder(NS(bot=self.bot), {1}, self.tmp.name+'/test.db')
        self.f.client = NS(forward_messages=AsyncMock(), send_message=AsyncMock(), get_messages=AsyncMock())
        self.f.db.execute("INSERT INTO rules(owner,source,target,source_label,target_label) VALUES(1,-1001,-1002,'src','dst')")
        self.f.db.commit()
    def tearDown(self):
        self.f.db.close(); self.tmp.cleanup()
    def test_filters_and_peers(self):
        self.assertTrue(accepted('hello', 'black', []))
        self.assertFalse(accepted('hello', 'white', []))
        self.assertFalse(accepted('Hello world', 'black', ['HELLO']))
        self.assertTrue(accepted('你好世界', 'white', ['世界']))
        self.assertEqual(peer('https://t.me/example'), '@example')
        self.assertEqual(peer('https://t.me/c/123'), -100123)
        for v in ['https://evil.test/x', 'https://t.me/example/12', 'https://t.me/+secret']:
            with self.assertRaises(ValueError): peer(v)
    async def test_dedup_and_filter(self):
        r = self.f.rule(1,1)
        msg=NS(id=5,chat_id=-1001,raw_text='hello',buttons=None,media=None)
        await self.f.deliver(r,msg);await self.f.deliver(r,msg)
        self.f.client.forward_messages.assert_awaited_once()
        self.f.db.execute("UPDATE rules SET filter='white' WHERE id=1")
        await self.f.deliver(self.f.rule(1,1),NS(id=6,chat_id=-1001,raw_text='hello',buttons=None))
        self.f.client.forward_messages.assert_awaited_once()
    async def test_link_expansion_and_callback(self):
        linked=NS(id=10,chat_id=-1003,raw_text='最终内容',buttons=None)
        self.f.client.get_messages.return_value=linked
        msg=NS(id=5,chat_id=-1001,raw_text='摘要',buttons=[[NS(url='https://t.me/example/10')]])
        self.assertIn('最终内容',await self.f.expanded(msg))
        click=AsyncMock(return_value=NS(message='展开文本'))
        msg.buttons=[[NS(url=None,data=b'x',text='查看内容',click=click)]]
        self.f.client.get_messages.return_value=msg
        with patch('forwarder.asyncio.sleep',AsyncMock()):
            self.assertIn('展开文本',await self.f.expanded(msg))
    async def test_keywords_modes_clear_and_permissions(self):
        message=NS(reply_text=AsyncMock())
        context=NS(user_data={})
        q=NS(from_user=NS(id=1),data='fw:mode:1',message=message)
        await self.f.button(NS(callback_query=q),context)
        self.assertEqual(self.f.rule(1,1)['mode'],'live')
        q.data='fw:edit:1';await self.f.button(NS(callback_query=q),context)
        await self.f.keyword_input(NS(effective_user=NS(id=1),effective_message=NS(text='abc\n你好',reply_text=AsyncMock())),context)
        self.assertIn('你好',self.f.rule(1,1)['words'])
        self.f.log(1,'test');q.data='fw:clear:1';await self.f.button(NS(callback_query=q),context)
        self.assertEqual(self.f.db.execute('SELECT count(*) FROM logs').fetchone()[0],0)
        with self.assertRaises(ValueError): self.f.rule(1,2)
    async def test_bot_destination(self):
        self.f.db.execute('UPDATE rules SET target=99 WHERE id=1')
        await self.f.deliver(self.f.rule(1,1),NS(id=5,chat_id=-1001,raw_text='hello',buttons=None,media=True))
        self.bot.forward_message.assert_awaited_once_with(1,-1001,5)
        self.bot.send_message.assert_awaited_once_with(1,'hello')

class AddedFeatures(unittest.IsolatedAsyncioTestCase):
    def test_http(self):
        self.assertEqual(app.parse_outbound_input('http://u:p%40ss@proxy.example:8080'),dict(type='http',server='proxy.example',server_port=8080,username='u',password='p@ss'))
        self.assertEqual(app.parse_outbound_input('http://proxy.example')['server_port'],80)
        self.assertTrue(app.parse_outbound_input('https://proxy.example')['tls']['enabled'])
        self.assertEqual(app.parse_outbound_input('socks://proxy.example:1080')['type'],'socks')
    async def test_snapshot_delete(self):
        msg=NS(reply_text=AsyncMock());ctx=NS(user_data={})
        snaps={'error':0,'snapshots':[{'fileName':'test'}]}
        api=AsyncMock(return_value=snaps)
        with patch.object(app,'bwh',api):
            await app.snapshot_buttons(msg,ctx,None,'snapview:'+app.snapshot_id('test'),1)
            self.assertEqual(api.await_count,1)
            cb=msg.reply_text.call_args.kwargs['reply_markup'].inline_keyboard[0][0].callback_data.split(':',1)[1]
            api.side_effect=[snaps,{'error':0}]
            await app.snapshot_buttons(msg,ctx,None,cb,1)
            self.assertEqual(api.call_args.args[1:3],('snapshot/delete',{'snapshot':'test'}))
            api.side_effect=None;api.return_value=snaps;api.reset_mock()
            await app.snapshot_buttons(msg,ctx,None,cb,1)
            self.assertEqual(api.await_count,1)
