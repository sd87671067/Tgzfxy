# Unified VLESS REALITY validation

The feature adds an opt-in multi-user listener and Telegram UUID → outbound →
confirmation flow. Existing listeners are not automatically migrated. This release
must not be interpreted as permission to take an occupied TCP 443 listener.

Formal validation ran in a newly cloned `sd87671067/go-emby-test` main workspace,
base `aa3e00898fbac4cdd204fd4763951167f5c07d3d`, with Tgzfxy changes in a separate
nested Git worktree. No go-emby application code or production license data was
modified. Only Tgzfxy sources are committed to this repository.

The targeted suite passed 25 top-level tests, including existing Bot and forwarding
regressions. The real integration test used sing-box 1.13.15, fresh synthetic
UUIDs/REALITY keys, loopback listeners, two mock SOCKS exits, a real HTTPS exit,
and temporary SQLite. No Telegram messages were sent.

Verified behavior:

- Separate UUIDs select separate exits through `auth_user` rules.
- Unknown UUIDs and authenticated users without bindings are rejected.
- REALITY + Vision reaches a real HTTPS IP endpoint with certificate verification.
- Actual Clash connection chains identify users independently of their shared exit.
- Bindings survive config reload; one user's pause leaves another user's exit usable.
- Candidate validation happens before active config replacement.
- Injected post-restart health failure restores config and the isolated service.
- Confirmation rejects expired/stale selections, another chat and replay.
- Limits, expiry, manual pause, user links and daily reports retain legacy behavior.
- Explicitly approved history links retain old records, inherit policy and share
  quota between old/new clients; failures restore changed policy/link state.
- Global route bypass, missing guards, invalid selectors and duplicate routes fail
  validation; referenced exits and populated unified listeners cannot be deleted.
- A private read-only migration fixture validates 17 proposed users while keeping
  every original listener, UUID, port and REALITY parameter unchanged. That
  candidate was checked but never started.

The v1.13.15 [Clash tracker serializer](https://github.com/SagerNet/sing-box/blob/v1.13.15/experimental/clashapi/trafficontrol/tracker.go)
does not expose authenticated username in JSON metadata. Per-user single-exit
selectors preserve attribution in `chains`; tests verify this against the binary,
not only fabricated API payloads.

Production cutover remains blocked by an occupied wildcard TCP 443 listener,
unapproved duplicate-UUID migration choices and outbound authentication failures.
The original HTTPS certificate, TLS version, root/health responses, protected
container identities/start times and sing-box configuration remained unchanged.
Private configuration, migration identities, detailed outbound diagnostics and
health snapshots remain outside Git.

Operational limits: Clash polling can miss connections entirely between polls;
it is not an exact billing source. Applying a binding reloads sing-box and existing
proxy connections reconnect. This release is code-only: it neither deploys a new
Bot nor changes production listeners, firewalls or authorization services.
