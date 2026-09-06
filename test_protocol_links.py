import base64
import unittest
from unittest.mock import patch
import app

class LinkTests(unittest.TestCase):
    def test_anytls_ipv6_encoded_password(self):
        x=app.parse_outbound_input('anytls://p%40ss%3Aword@[::1]:443/?sni=example.com&insecure=0#test')
        self.assertEqual(x['password'],'p@ss:word'); self.assertEqual(x['server'],'::1'); self.assertFalse(x['tls']['insecure'])
    def test_hy2_userpass_obfs(self):
        x=app.parse_outbound_input('hy2://user:pass@example.com?obfs=salamander&obfs-password=x%26y&alpn=h3')
        self.assertEqual(x['password'],'user:pass'); self.assertEqual(x['obfs']['password'],'x&y'); self.assertEqual(x['server_port'],443)
    def test_reality(self):
        x=app.parse_outbound_input('vless://550e8400-e29b-41d4-a716-446655440000@example.com:443?security=reality&pbk='+base64.urlsafe_b64encode(bytes(range(32))).decode().rstrip('=')+'&sid=ab12&flow=xtls-rprx-vision&sni=example.org&fp=chrome')
        self.assertEqual(x['tls']['reality']['short_id'],'ab12'); self.assertEqual(x['flow'],'xtls-rprx-vision')
    def test_bad_links(self):
        for x in ['anytls://x@example.com:0','anytls://x@example.com:65536','anytls://example.com:443','vless://bad@example.com?security=reality','hy2://x@example.com?obfs=salamander','hy2://x@example.com?mport=600-500','anytls://x@example.com?insecure=maybe']:
            with self.subTest(x=x),self.assertRaises(ValueError): app.parse_outbound_input(x)
    def test_socks_form_and_uri(self):
        self.assertEqual(app.parse_outbound_input('server: example.com\nport: 1080\nuser: u\npass: p')['type'],'socks')
        self.assertEqual(app.parse_outbound_input('socks5://u:p@example.com:1080')['username'],'u')
    def test_new_tag_types(self):
        d={}
        for typ in ['anytls','vless','hysteria2']:
            tag=app.add_outbound_to_doc(d,{'type':typ,'server':'example.com','server_port':443})
            self.assertTrue(tag.startswith(typ+'-')); self.assertEqual(d['outbounds'][-1]['type'],typ)
    def test_share_tls_verification_and_legacy_domain(self):
        d={'inbounds':[{'type':t,'tag':t,'listen_port':443,'users':[{'password':'p'}], 'tls':{'server_name':'legacy.example.com' if t=='anytls' else app.SHARE_DOMAIN}} for t in ['anytls','hysteria2']]}
        with patch.object(app,'load_config',return_value=d),patch.object(app,'server_host',return_value='127.0.0.1'):
            sub,errors=app.share_links(['anytls','hysteria2'])
        self.assertFalse(errors); links=base64.b64decode(sub).decode(); self.assertIn('sni=legacy.example.com',links); self.assertIn('sni='+app.SHARE_DOMAIN,links); self.assertNotIn('insecure=1',links)
    def test_menu_label(self):
        labels=[b.text for row in app.proto_menu_keyboard().inline_keyboard for b in row]
        self.assertIn('➕ 添加出站',labels)

if __name__=='__main__': unittest.main()
