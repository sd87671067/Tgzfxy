# Unified VLESS REALITY validation

The feature adds an opt-in multi-user listener and Telegram UUID → outbound →
confirmation flow. Existing listeners are not automatically migrated. This release
must not be interpreted as permission to take an occupied TCP 443 listener.

Formal validation ran in a newly cloned `sd87671067/go-emby-test` main workspace,
base `aa3e00898fbac4cdd204fd4763951167f5c07d3d`, with Tgzfxy changes in a separate
nested Git worktree. No go-emby application code or production license data was
modified. Only Tgzfxy sources are committed to this repository.

The targeted suite passed 26 top-level tests, including existing Bot and forwarding
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

## Shared TCP 443 deployment

The administrator panel includes a unified entry menu. Binding follows user →
change outbound → healthy outbound and observed exit IP → confirmation. The
production ConfigManager probes each candidate using a temporary loopback
sing-box client, checks the chosen exit again before applying, then performs a
REALITY/Vision request as that user through the public SNI listener. An exit IP
mismatch or failed request restores the previous configuration and service.
Probes never change user limits, expiry, pause state or accounting history.

Host prerequisites for these probes are Python 3, curl and sing-box; generated
probe configurations have mode 0600 and are removed along with their subprocesses.
Unreachable exits are omitted from binding choices, but are not automatically
deleted: deletion requires repeated authentication failures and a reviewed private
manifest. Production credentials, UUIDs, links, mappings and backups stay off Git.

HAProxy must use TCP TLS passthrough with distinct license and REALITY SNI ACLs.
Bind its public IPv4 and IPv6 TCP 443 listeners, send license SNI to the gateway's
loopback HTTPS port and REALITY SNI to the sole loopback VLESS listener, and reject
unknown SNI. Test this routing on a temporary loopback port before moving the
license gateway's Docker 443 mapping. Preserve certificate mounts, database,
network and environment. Do not send PROXY protocol to an unprepared backend.
The backend sees the proxy address; source-IP policy requires separate review.

Keep old healthy VLESS listeners until every retained UUID succeeds through public
443. Then remove legacy VLESS listeners and their route references. Preserve other
protocols and unrelated services. Copy legacy policy and attach history by user
key in a transaction; retain samples and audit rows. Rollback removes only the
migration's new policy and mapping rows, never overwrites a live database snapshot.
