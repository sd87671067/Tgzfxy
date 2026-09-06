import copy
import unittest

import app


class SocksOutboundTests(unittest.TestCase):
    def test_parse_socks5_input_accepts_requested_format(self):
        text = """Proxy server: 192.0.2.10
port: 6011
username: example-user
password: example-password"""
        self.assertEqual(
            app.parse_socks5_input(text),
            {
                "server": "192.0.2.10",
                "server_port": 6011,
                "username": "example-user",
                "password": "example-password",
            },
        )

    def test_parse_socks5_input_rejects_missing_password(self):
        with self.assertRaisesRegex(ValueError, "password"):
            app.parse_socks5_input("Proxy server: 1.2.3.4\nport: 1080\nusername: u")

    def test_add_socks_outbound_uses_unique_random_tag(self):
        doc = {"outbounds": [{"type": "direct", "tag": "direct"}, {"type": "socks", "tag": "socks-aabbccdd"}]}
        values = {"server": "192.0.2.10", "server_port": 6011, "username": "u", "password": "p"}
        tag = app.add_socks_outbound_to_doc(doc, values, tag_factory=lambda: "socks-11223344")
        self.assertEqual(tag, "socks-11223344")
        self.assertEqual(doc["outbounds"][-1], {"type": "socks", "tag": tag, **values})


class RouteBindingTests(unittest.TestCase):
    def setUp(self):
        self.doc = {
            "inbounds": [{"type": "vless", "tag": "in-a"}, {"type": "vless", "tag": "in-b"}],
            "outbounds": [{"type": "direct", "tag": "direct"}, {"type": "socks", "tag": "socks-a"}],
            "route": {"rules": [{"inbound": ["in-a"], "outbound": "direct"}]},
        }

    def test_validate_binding_rejects_two_inbounds(self):
        with self.assertRaisesRegex(ValueError, "一个入站.*一个出站"):
            app.validate_binding_selection(self.doc, "in-a", "in-b")

    def test_validate_binding_rejects_two_outbounds(self):
        with self.assertRaisesRegex(ValueError, "一个入站.*一个出站"):
            app.validate_binding_selection(self.doc, "direct", "socks-a")

    def test_bind_route_replaces_existing_rule_without_duplicates(self):
        app.bind_route_in_doc(self.doc, "in-a", "socks-a")
        matching = [r for r in self.doc["route"]["rules"] if "in-a" in r.get("inbound", [])]
        self.assertEqual(matching, [{"inbound": ["in-a"], "outbound": "socks-a"}])

    def test_bind_route_preserves_limit_block_rule_first(self):
        self.doc["route"]["rules"].insert(0, {"inbound": ["in-b"], "outbound": app.MANAGED_BLOCK})
        app.bind_route_in_doc(self.doc, "in-a", "socks-a")
        self.assertEqual(self.doc["route"]["rules"][0]["outbound"], app.MANAGED_BLOCK)


class DeletionTests(unittest.TestCase):
    def setUp(self):
        self.doc = {
            "inbounds": [{"type": "vless", "tag": "in-a"}, {"type": "vless", "tag": "in-b"}],
            "outbounds": [{"type": "direct", "tag": "direct"}, {"type": "socks", "tag": "out-a"}, {"type": "socks", "tag": "out-b"}],
            "route": {"rules": [
                {"inbound": ["in-a"], "outbound": "out-a"},
                {"inbound": ["in-b"], "outbound": "out-b"},
            ]},
        }

    def test_delete_inbound_removes_its_route_rule(self):
        app.delete_tag_from_doc(self.doc, "inbound", "in-a")
        self.assertFalse(any(x.get("tag") == "in-a" for x in self.doc["inbounds"]))
        self.assertFalse(any("in-a" in r.get("inbound", []) for r in self.doc["route"]["rules"]))

    def test_delete_outbound_remaps_affected_inbound_to_direct(self):
        app.delete_tag_from_doc(self.doc, "outbound", "out-a")
        self.assertFalse(any(x.get("tag") == "out-a" for x in self.doc["outbounds"]))
        rule = next(r for r in self.doc["route"]["rules"] if "in-a" in r.get("inbound", []))
        self.assertEqual(rule["outbound"], "direct")

    def test_delete_direct_outbound_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "direct"):
            app.delete_tag_from_doc(self.doc, "outbound", "direct")


class RealityTests(unittest.TestCase):
    def test_new_reality_defaults_to_itunes(self):
        self.assertEqual(app.REALITY_SNI, "itunes.apple.com")


class ExpiryTests(unittest.TestCase):
    def test_parse_duration_days_numbers(self):
        self.assertEqual(app.parse_duration_days("30"), 30)
        self.assertEqual(app.parse_duration_days("0"), None)
        self.assertEqual(app.parse_duration_days(""), None)
        self.assertEqual(app.parse_duration_days(None), None)

    def test_parse_duration_days_compound_units(self):
        self.assertEqual(app.parse_duration_days("1y"), 365)
        self.assertEqual(app.parse_duration_days("3m"), 90)
        self.assertEqual(app.parse_duration_days("2w"), 14)
        self.assertEqual(app.parse_duration_days("7d"), 7)

    def test_parse_duration_days_keywords(self):
        self.assertIsNone(app.parse_duration_days("permanent"))
        self.assertIsNone(app.parse_duration_days("永久"))

    def test_parse_duration_days_rejects_garbage(self):
        with self.assertRaisesRegex(ValueError, "无法解析"):
            app.parse_duration_days("abc")

    def test_expiry_timestamp(self):
        self.assertIsNone(app.expiry_timestamp(None))
        ts = app.expiry_timestamp(1)
        self.assertGreater(ts, int(app.datetime.now(app.TZ).timestamp()))


if __name__ == "__main__":
    unittest.main()
