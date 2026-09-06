import asyncio
import time
from types import SimpleNamespace as NS
from unittest.mock import AsyncMock, Mock
from telethon import errors
from test_forwarder import ForwardTests

class LoginAndRulesTests(ForwardTests):
    def setUp(self):
        super().setUp()
        self.f.client = NS(is_connected=Mock(return_value=True), connect=AsyncMock(), get_me=AsyncMock(return_value=None), is_user_authorized=AsyncMock(return_value=False), add_event_handler=Mock(), send_code_request=AsyncMock(return_value=NS(phone_code_hash='hash')), sign_in=AsyncMock())
        self.message = NS(chat=NS(type='private'), text='', reply_text=AsyncMock(), delete=AsyncMock())
        self.ctx = NS(user_data={})
        self.update = NS(effective_user=NS(id=1), effective_message=self.message, callback_query=NS(from_user=NS(id=1), message=self.message, data='fw:menu'))
    async def test_phone_code_success(self):
        await self.f.button(self.update, self.ctx)
        self.assertEqual(self.ctx.user_data['fw_login']['step'],'phone')
        self.message.text='invalid';await self.f.login_input(self.update,self.ctx)
        self.f.client.send_code_request.assert_not_awaited()
        self.message.text='+12025550123';await self.f.login_input(self.update,self.ctx)
        self.assertEqual(self.ctx.user_data['fw_login']['step'],'code')
        self.f.client.get_me.return_value=NS(id=123)
        self.message.text='1 2 3 4 5';await self.f.login_input(self.update,self.ctx)
        self.f.client.sign_in.assert_awaited_once_with(phone='+12025550123',code='12345',phone_code_hash='hash')
        self.assertTrue(self.f.ready)
        self.assertNotIn('fw_login',self.ctx.user_data)
    async def test_existing_session(self):
        self.f.client.get_me.return_value=NS(id=123)
        await self.f.button(self.update,self.ctx)
        self.assertTrue(self.f.ready)
        self.assertNotIn('fw_login',self.ctx.user_data)
    async def test_2fa_and_bad_password(self):
        await self.f.button(self.update,self.ctx)
        self.message.text='+12025550123';await self.f.login_input(self.update,self.ctx)
        self.f.client.sign_in.side_effect=errors.SessionPasswordNeededError(None)
        self.message.text='1 2 3 4 5';await self.f.login_input(self.update,self.ctx)
        self.assertEqual(self.ctx.user_data['fw_login']['step'],'password')
        self.f.client.sign_in.side_effect=errors.PasswordHashInvalidError(None)
        self.message.text='wrong';await self.f.login_input(self.update,self.ctx)
        self.assertEqual(self.ctx.user_data['fw_login']['step'],'password')
        self.f.client.sign_in.side_effect=None;self.f.client.get_me.return_value=NS(id=123)
        self.message.text=' secret ';await self.f.login_input(self.update,self.ctx)
        self.f.client.sign_in.assert_awaited_with(password=' secret ')
        self.assertTrue(self.f.ready)
    async def test_expired_invalid_and_flood(self):
        await self.f.button(self.update,self.ctx)
        self.message.text='+12025550123';await self.f.login_input(self.update,self.ctx)
        self.message.text='1 2 3 4 5'
        self.f.client.sign_in.side_effect=errors.PhoneCodeInvalidError(None)
        await self.f.login_input(self.update,self.ctx)
        self.assertEqual(self.ctx.user_data['fw_login']['step'],'code')
        self.f.client.sign_in.side_effect=errors.PhoneCodeExpiredError(None)
        await self.f.login_input(self.update,self.ctx)
        self.assertEqual(self.ctx.user_data['fw_login']['step'],'phone')
        self.f.client.send_code_request.side_effect=errors.FloodWaitError(None, capture=30)
        self.message.text='+12025550123';await self.f.login_input(self.update,self.ctx)
        self.assertIn('30',self.message.reply_text.call_args.args[0])
        self.ctx.user_data['fw_login']['expires']=0
        await self.f.login_input(self.update,self.ctx)
        self.assertNotIn('fw_login',self.ctx.user_data)
    async def test_private_and_owner_guards(self):
        self.message.chat.type='group';await self.f.button(self.update,self.ctx)
        self.f.client.get_me.assert_not_awaited()
        self.assertNotIn('fw_login',self.ctx.user_data)
        self.message.chat.type='private';self.f.login_owner=2;self.f.login_until=time.time()+600
        await self.f.button(self.update,self.ctx)
        self.assertNotIn('fw_login',self.ctx.user_data)
    async def test_list_select_delete(self):
        self.update.callback_query.data='fw:switch';await self.f.button(self.update,self.ctx)
        self.assertIn('fw:rule:1',str(self.message.reply_text.call_args.kwargs))
        self.update.callback_query.data='fw:rule:1';await self.f.button(self.update,self.ctx)
        self.assertIn('fw:delete:1',str(self.message.reply_text.call_args.kwargs))
        self.f.log(1,'log');self.f.db.execute('insert into sent values(1,9)');self.f.db.commit()
        self.update.callback_query.data='fw:delete:1';await self.f.button(self.update,self.ctx)
        self.assertEqual(len(self.f.rows()),1)
        self.update.callback_query.data='fw:confirm_delete:1';await self.f.button(self.update,self.ctx)
        self.assertEqual(len(self.f.rows()),0)
        self.assertEqual(self.f.db.execute('select count(*) from sent').fetchone()[0],0)
        self.assertEqual(self.f.db.execute('select count(*) from logs').fetchone()[0],0)
        await self.f.button(self.update,self.ctx)
        self.assertIn('规则不存在',self.message.reply_text.call_args.args[0])
    async def test_delete_waits_for_forward(self):
        lock=self.f.locks.setdefault(1,asyncio.Lock());await lock.acquire()
        self.update.callback_query.data='fw:confirm_delete:1'
        task=asyncio.create_task(self.f.button(self.update,self.ctx))
        await asyncio.sleep(0)
        self.assertEqual(len(self.f.rows()),1)
        lock.release();await task
        self.assertEqual(len(self.f.rows()),0)
    async def test_pagination_and_permissions(self):
        for i in range(25):
            self.f.db.execute("INSERT INTO rules(owner,source,target,source_label,target_label) VALUES(1,1,2,'a','b')")
        self.f.db.execute("INSERT INTO rules(owner,source,target,source_label,target_label) VALUES(2,1,2,'PRIVATE','PRIVATE')")
        self.f.db.commit()
        await self.f.list_rules(self.message,1)
        markup=str(self.message.reply_text.call_args.kwargs)
        self.assertNotIn('PRIVATE',markup);self.assertIn('fw:list:1',markup)
        self.update.callback_query.data='fw:confirm_delete:27';await self.f.button(self.update,self.ctx)
        self.assertEqual(len(self.f.rows()),27)
    # Base forwarding tests use their original client doubles.
    async def test_dedup_and_filter(self):
        self.f.client.forward_messages=AsyncMock()
        await super().test_dedup_and_filter()
    async def test_link_expansion_and_callback(self):
        self.f.client.get_messages=AsyncMock()
        await super().test_link_expansion_and_callback()
