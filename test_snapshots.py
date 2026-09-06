import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch
import app

class Snapshots(unittest.IsolatedAsyncioTestCase):
    async def test_list_restore_create_and_errors(self):
        filename = 'snapshot-' + 'a' * 160
        snapshots = {'error': 0, 'snapshots': [{'fileName': filename, 'description': 'manual'}]}
        message = SimpleNamespace(reply_text=AsyncMock())
        context = SimpleNamespace(user_data={})
        api = AsyncMock(return_value=snapshots)
        async def click(sub):
            await app.snapshot_buttons(message, context, None, sub, 1)
        with patch.object(app, 'bwh', api):
            await click('snap')
            keyboard = message.reply_text.call_args.kwargs['reply_markup']
            for row in keyboard.inline_keyboard:
                for button in row:
                    self.assertLessEqual(len(button.callback_data.encode()), 64)
            restore = keyboard.inline_keyboard[0][1].callback_data.split(':', 1)[1]
            api.reset_mock()
            await click(restore)
            self.assertEqual([c.args[1] for c in api.call_args_list], ['snapshot/list'])
            confirm = message.reply_text.call_args.kwargs['reply_markup'].inline_keyboard[0][0].callback_data.split(':', 1)[1]
            api.side_effect = [snapshots, {'error': 0}]
            await click(confirm)
            self.assertEqual(api.call_args.args[1:3], ('snapshot/restore', {'snapshot': filename}))
            api.side_effect = None
            api.return_value = snapshots
            api.reset_mock()
            await click(confirm)
            self.assertEqual(api.await_count, 1)
            api.reset_mock()
            api.return_value = {'error': 0}
            await click('snapnew')
            self.assertEqual([c.args[1] for c in api.call_args_list], ['snapshot/create'])
            api.return_value = {'error': 0, 'snapshots': []}
            await click('snap')
            self.assertIn('当前没有快照', message.reply_text.call_args.args[0])
            api.return_value = {'error': 1, 'message': 'failure'}
            await click('snap')
            self.assertIn('失败', message.reply_text.call_args.args[0])

    async def test_pagination_and_stale_buttons(self):
        message = SimpleNamespace(reply_text=AsyncMock())
        context = SimpleNamespace(user_data={})
        api = AsyncMock(return_value={'error': 0, 'snapshots': [{'fileName': str(i)} for i in range(20)]})
        with patch.object(app, 'bwh', api):
            await app.snapshot_buttons(message, context, None, 'snappage:1', 1)
            self.assertIn('第 2 页', message.reply_text.call_args.args[0])
            await app.snapshot_buttons(message, context, None, 'snaprestore:missing', 1)
            self.assertIn('失效', message.reply_text.call_args.args[0])

if __name__ == '__main__':
    unittest.main()
