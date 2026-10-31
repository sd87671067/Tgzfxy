# Shared direct outbound and user menus

Each unified user keeps its own UUID, name, SQLite accounting key, quota,
expiry and `inbound` + `auth_user` route. Direct users route to the existing
`direct` outbound without an intermediate selector. Proxy users retain their
dedicated selectors. Changing one user's binding only replaces that user's
route and selector. All save paths normalize old managed direct aliases.

The unified inbound's unconditional reject rule is mandatory and follows all
user routes. Unbound authenticated users cannot reach `route.final`, even if
the legacy configuration uses `final: direct`. Unknown UUIDs also fail VLESS
authentication. The Shadowsocks inbound and its existing routing are retained.

sing-box 1.13.15 Clash metadata lacks an authenticated username. Direct traffic
is attributed using the exact matched rule
`inbound=<inbound> auth_user=<name> => route(direct)` and the direct chain.
Names, inbound identity and complete rule text must match; a shared chain alone
never attributes traffic. Existing selector-based accounting remains compatible.
The real REALITY integration test verifies both forms and independent direct
users against the installed sing-box binary. Re-run it before core upgrades.

`/users` uses readable `name → outbound` buttons, eight users per page, a page
indicator and previous/next navigation. Each page rotates a short session token.
Callbacks bind to chat, message, configuration version and a five-minute expiry.
Changed configurations cannot reuse old user selections; page navigation reloads
the current list and clamps the page number. Adding users and saving a binding
refresh the list. Confirmation is consumed before writing; failed checks,
reloads or exit verification restore the previous configuration.

To stage a migration, run `sbbot-normalize -input <config> -output <new-path>`.
The command refuses to overwrite an existing candidate and never touches SQLite
or reloads services. Check the candidate with `sing-box check` before installation.
Back up configuration, Compose and SQLite through its backup API first. Deploy
the candidate together with the new Bot image: the old Bot rejects direct user
routes. Rollback must restore both configuration and image, retaining live
database history rather than restoring an outdated database snapshot.
