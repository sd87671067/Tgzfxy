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
production ConfigManager probes the selected candidate using a temporary loopback
sing-box client, checks the chosen exit again before applying, then performs a
REALITY/Vision request as that user through the public SNI listener. An exit IP
mismatch or failed request restores the previous configuration and service.
Probes never change user limits, expiry, pause state or accounting history.

Host prerequisites for these probes are Python 3, curl and sing-box; generated
probe configurations have mode 0600 and are removed along with their subprocesses.
Outbound choices are paginated; unreachable exits fail their selection probe, but are not automatically
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


## Unified management and independent cycle policy

The home menu's unified entry owns users, outbound binding, REALITY camouflage,
per-user shares, traffic/expiry and listener settings. Protocol management retains
its original commands and legacy protocol features, without duplicate user buttons.
User, outbound, binding, traffic and share selections use eight items per page.
Selection callbacks carry short references and expiring tokens bound to the
administrator session, chat, message and configuration version. Navigation edits
the existing message; mutations consume confirmations and refresh the list.

`/share zs` resolves a unique unified user name; full user keys and all existing
multi-tag/protocol shares remain supported. Exact legacy inbound tags retain
precedence when a short name collides. Duplicate unified names require full keys.
The VLESS URL uses the public port, UUID, REALITY public key, SNI, short ID and
Vision flow. Copy instructions describe long-press and v2rayN clipboard import;
ordinary Telegram buttons do not access the system clipboard.

`/alllimit zs` grants 512000 MiB for 30 days; `/alllimit zs 2048 7` grants 2048 MiB
for seven days. Positive integer validation and checked byte conversion precede
all writes. The persisted `user_cycles` record stores lifetime-usage baseline,
allowance, start and expiry. Remaining cycle traffic is allowance minus the
increase in the existing lifetime samples, including explicitly adopted legacy
history. Resetting the cycle takes a new baseline and never deletes samples or
resets connection counters. Month boundaries do not reset a cycle. Existing
persisted monthly quotas, `/setexpiry`, manual pauses and legacy relationships remain
independent constraints; renewing a cycle does not remove those restrictions.
The collector applies scoped auth_user rejects at its normal polling interval;
Clash sampling remains an estimate and may miss very short connections. SQLite
is backed up before setting a cycle; failed config applications restore the old
policy. Persisted desired policy is re-enforced by collection after restart.

REALITY editing accepts `domain` (SNI and handshake:443) or
`SNI-domain handshake-domain:port`. DNS, public addresses, certificate validation,
TLS 1.3 and h2 are required before preview and again on confirmation. Literal
HAProxy SNI collisions and unsupported complex ACLs fail closed. Application
checks a sing-box candidate, backs up configs, updates only the managed HAProxy
REALITY ACL, validates/reloads HAProxy, and probes an existing UUID through the
public listener. Failure restores sing-box and HAProxy; UUIDs, keys and short IDs
are retained. Host Python3, curl, HAProxy and systemd are prerequisites.

Formal tests run only in an isolated `sd87671067/go-emby-test` checkout. Directed
coverage includes parser bounds, clipboard URL fields, session/replay guards,
partial pages, cycle persistence/month independence, policy/config rollback,
HAProxy reload rollback, and real sing-box REALITY/Vision UUID isolation including
cycle exhaustion, renewal and expiry. No full or race suite is required.
