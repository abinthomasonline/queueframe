# Queueframe

Queueframe is a private, self-hosted web application for staging Zerodha Kite
Connect orders and starting their placement requests at a configured time in
`Asia/Kolkata`. It is designed for one person or a Zerodha-permitted immediate
family running one application instance—not as a public trading service.

The default first request-start target is `09:00:00 IST`. This is a local
scheduler target, not a guarantee of Zerodha OMS acceptance, exchange receipt,
matching, or execution. Queueframe never discovers a price, holding, balance,
quantity, circuit, or trading opportunity. Every broker parameter comes from
the user.

## What is implemented

- Go server, server-rendered responsive UI, SQLite, and the official
  `gokiteconnect/v4` client.
- Phone layouts for every public, trader and administrator page, including
  touch queue controls, card-style data tables, a scrollable modal navigation
  drawer, safe-area-aware bottom navigation and millisecond IST clock.
- Administrator-invited first-time signup with a short-lived, single-use link,
  Zerodha client ID, API key, API secret, the deployment's static egress IP,
  its exact Kite callback URL, and a mandatory household/IP acknowledgement.
- Returning-user sign-in with only the client ID followed by interactive Kite
  login. Credentials and access tokens are encrypted at rest and never sent
  back to browser JavaScript.
- A completely separate local administrator identity. Administrator login is
  available even during the trader-login blackout.
- Daily activation only after successful Kite login at or after `06:00 IST` on
  the execution date.
- Searchable in-memory validation against Zerodha's current NSE/BSE equity
  instrument master. No quote, price, holding, or balance lookup is performed.
- Regular NSE/BSE `LIMIT` and `MARKET` orders with fixed `DAY` validity. Market
  protection is not a user field: Queueframe sends Zerodha's automatic `-1`
  setting for MARKET and `0` for LIMIT.
- Dense per-trader/date queues with drag/button reordering, optimistic revision
  checks, deletion, and recalculated per-order request targets.
- Versioned administrator settings for dispatch time, preparation lead, trader
  login blackout, batch interval, and maximum orders per trader/date.
- Dispatch/outcome history for traders and a global administrator view,
  including planned/start/finish times, OMS/exchange identifiers and broker
  status when Kite exposes them.
- Local profile deletion that erases credentials and mutable future orders
  while retaining redacted dispatch/audit history. It does not close a Zerodha
  account or delete a Kite Connect app.
- Dry-run placement by default.

## Timing and failure model

At the configured preparation cutoff (five minutes before dispatch by
default), Queueframe freezes that execution date and loads every credential,
client, tag, payload, position and target into memory. SQLite and application
logging are kept out of the placement hot path.

Each trader's positions 1–10 target the configured base time. Positions 11–20
target base time plus the configured batch interval (1.05 seconds by default),
and so on. Workers for a batch are created before its absolute target and are
released concurrently. A slow or failed request in one batch does not delay a
later batch's independent timer. Concurrency does not guarantee relative
exchange ordering among requests in the same batch. The configurable queue cap
cannot exceed Zerodha's documented 400 order requests per minute.

Every order-placement request is attempted **at most once**. There is no retry
for 429, timeout, network error, 5xx, or any other placement result. A definite
rejection is recorded as rejected. An ambiguous result is recorded `UNKNOWN`;
Queueframe may then perform read-only order-book reconciliation, but it never
issues a second placement request. HTTP redirects are not followed, preventing
Go's client from replaying a POST across a 307/308 response. A unique tagged
order found during reconciliation proves one request reached Kite; otherwise a
restart-ambiguous record says its request start is unknown. A trader without a
valid same-day activation is recorded `MISSED_INACTIVE` with request count zero.

`ACKNOWLEDGED` means the initial Kite OMS response was accepted. When the
bounded read-only monitor observes `COMPLETE`, `REJECTED`, or `CANCELLED`, that
terminal broker outcome replaces the acknowledgement in the durable history.

## Household and static-IP restriction

Zerodha currently requires a whitelisted static public egress IP for API order
requests. Its documented sharing exception is limited to a spouse, dependent
children, and dependent parents using one developer account with separate apps
and client IDs. It is not a general friends, colleagues, siblings, or public
multi-tenant exception. Review the current
[Zerodha static-IP guidance](https://support.zerodha.com/category/trading-and-markets/general-kite/kite-api/articles/static-ip)
before deployment.

Signup records this exact acknowledgement:

> I confirm that this application instance is hosted exclusively for my use
> and/or the use of my immediate family members permitted by Zerodha (my
> spouse, dependent children, and dependent parents), and that its displayed
> static egress IP is not shared with or used for API order placement by anyone
> outside this group.

`QUEUEFRAME_EXPECTED_PUBLIC_IP` is trusted deployment configuration displayed
to the user. Queueframe does not call an external “what is my IP” service and
cannot prove that Zerodha's whitelist matches it. New signup is disabled while
the value is missing and is available only through an unexpired administrator
invitation.

## Prerequisites

- Go 1.26.5 or Docker/Compose. Queueframe tracks a supported, patched Go
  toolchain because its standard-library HTTP, TLS and template packages are
  part of the application's security boundary.
- One TOTP-enabled Zerodha account and one Kite Connect app per connected
  client ID.
- A stable outbound public IP entered in the Kite developer profile.
- The Kite app redirect URL set exactly to:

  ```text
  <QUEUEFRAME_BASE_URL>/auth/kite/callback
  ```

- A host synchronised with NTP and a local persistent filesystem for SQLite.
- HTTPS for any non-loopback browser access.

For one trader and one order, a small VPS with **1 shared vCPU, 1 GB RAM and
10 GB local SSD** is ample. Stable egress, low jitter to Kite, a static IP and
correct clock synchronisation matter more than additional CPU. A 512 MB host
can work, but 1 GB leaves comfortable room for the Go process, SQLite, reverse
proxy and operating system.

## Choose an access mode

| Mode | Best for | Domain | Browser access |
| --- | --- | --- | --- |
| Domain + Caddy | A convenient household instance | Required | Public trusted HTTPS |
| Tailscale Serve | A private instance without a domain | Not required | Trusted HTTPS inside one tailnet |
| Public IP certificate | Advanced no-domain fallback | Not required | Public trusted HTTPS at the numeric IP |

Domain + Caddy is the recommended default. Tailscale Serve is the recommended
no-domain option when family members can join one tailnet. Numeric-IP
certificates are supported but expire after only 160 hours and require
continuous automated renewal.

Start with the full [deployment guide](docs/deployment.md). A reusable
[Google Cloud guide](docs/gcp.md) covers an isolated `gcloud` configuration,
Mumbai VM, reserved egress address, IAP-only SSH, budget alerts, and deployment
checks without embedding any operator-specific project or domain.

## Local quick start

```sh
cp .env.example .env
chmod 600 .env
openssl rand -base64 32
```

Paste the generated value into `QUEUEFRAME_MASTER_KEY`, set an administrator
password of at least 12 characters, and set the expected public egress IP.
Then:

```sh
set -a
. ./.env
set +a
go run ./cmd/queueframe
```

Open <http://localhost:8080>. Keep `QUEUEFRAME_DRY_RUN=true` through onboarding
and at least one complete rehearsal. Dry-run still uses real Kite login,
profile and instrument-master reads, but replaces placement with a synthetic
local acknowledgement. Sign in at <http://localhost:8080/admin/login>, open
`/admin/invites`, and create a single-use link before first-time trader setup.

## Production Compose

The production deployment has interactive configuration and three explicit
profiles:

```sh
./deploy/setup.sh
./deploy/doctor.sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml up --build -d
./deploy/doctor.sh --running
# After the first administrator sign-in succeeds:
./deploy/seal.sh
```

`setup.sh` creates `deploy/.env` with mode `600`, generates the master key,
selects `domain`, `tailscale`, or `public-ip`, and leaves dry-run enabled. It
refuses to overwrite an existing file. Queueframe has no host-published port in
this Compose stack; Caddy or Tailscale is the only browser entry point. After
the first administrator login succeeds, `seal.sh` verifies the running ingress
and atomically removes the plaintext bootstrap password and any one-off
Tailscale auth key from `deploy/.env`; it leaves the master key intact.

See [deployment](docs/deployment.md), [security and invitations](docs/security.md),
and [backup/update operations](docs/operations.md) before putting the instance
on the internet or enabling live placement.

GitHub CI verifies formatting, module tidiness, vet, normal and race tests,
reachable Go vulnerabilities, deployment assets, and the container build. A
SemVer `v*` tag publishes a provenance- and SBOM-enabled `amd64`/`arm64` image
at `ghcr.io/abinthomasonline/queueframe`. Repository and package visibility are
separate: the first private review keeps both private. Before making the source
public, choose and add an open-source license and separately make the GHCR
package public if anonymous pulls should work. See the [release checklist](docs/releasing.md)
and report security problems through [the private disclosure process](SECURITY.md).

## Loopback Compose

```sh
docker compose up --build -d
docker compose logs -f queueframe
curl http://127.0.0.1:8080/healthz
```

The root development Compose file binds to loopback, runs as non-root with a
read-only root filesystem, and stores SQLite in `queueframe-data`. It is useful
for local development and the documented SSH-tunnel recovery path. Use the
production Compose profiles for routine remote access.

Run exactly one Queueframe process/replica against a database. Do not put
SQLite on NFS, SMB, or a shared network volume. Back up the named volume and
master key separately; losing the key makes encrypted credentials
unrecoverable.

## User workflow

1. An administrator signs in locally, opens `/admin/invites`, and sends one
   short-lived, single-use invitation privately to a permitted household
   member.
2. The recipient opens the invitation and enters their Zerodha client ID, API
   key and API secret. Follow the displayed instructions to whitelist the
   egress IP and register the exact redirect URL.
3. Accept the household/static-IP acknowledgement and complete Kite login.
   Queueframe accepts signup only when Kite returns the submitted client ID and
   API key and the profile read confirms that identity. A successful callback
   consumes the invitation.
4. Create orders for an unfrozen weekday, selecting a symbol from the validated
   instrument search. Reorder them to choose their batch/position.
5. On the execution date, complete Kite login after `06:00 IST` and before the
   configured blackout. A previous day's local login is not activation.
6. Keep the instance running before preparation through result persistence,
   then review **Dispatches**. `ACKNOWLEDGED` means an OMS acknowledgement, not
   necessarily an exchange fill; inspect the broker status and identifiers.

Access tokens normally expire at `06:00 IST` the following day and can be
invalidated earlier by logout. Queueframe does not automate Zerodha login or
TOTP.

## Order caveats

Queueframe does not validate funds, holdings, available quantity, tick or lot
size, freeze quantity, circuit/LPP limits, exchange holidays, product
eligibility, or surveillance status. Those checks remain with Zerodha and the
exchange and can reject an order.

Some surveillance/ASM orders require consent that Kite Connect cannot collect;
place those manually on Kite when required. For delivery sells, DDPI is
optional, but without DDPI the user may need separate same-day CDSL TPIN
authorisation. Queueframe performs neither flow. Zerodha's current online DDPI
instructions are available in its
[DDPI activation guide](https://support.zerodha.com/category/your-zerodha-account/your-profile/ddpi/articles/activate-ddpi).

## Administrator settings

The root administrator cannot be deleted or demoted. The dashboard allows the
administrator to:

- see/delete local trader profiles;
- create, inspect and revoke short-lived single-use enrollment invitations;
- inspect, reorder, edit or delete every unfrozen order;
- inspect every dispatch/outcome;
- schedule an immutable settings version for a future weekday; and
- change the administrator password, revoking every admin web session.

Defaults are dispatch `09:00:00`, preparation lead `5m`, trader-login blackout
`[08:45:00, 09:30:00)`, batch interval `1050ms`, and 100 orders per
trader/date. Timezone, activation boundary, ten-order batch size, one placement
attempt and `DAY` validity are fixed. A settings version is rejected unless its
blackout covers preparation through the final possible batch and still leaves
a non-empty login window after `06:00 IST` and before preparation.

## Deployment configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `QUEUEFRAME_MASTER_KEY` | required | Standard-base64 encoding of exactly 32 random bytes. |
| `QUEUEFRAME_ADMIN_PASSWORD` | required on empty DB | Bootstrap root-admin password, minimum 12 characters. |
| `QUEUEFRAME_ADMIN_USER` | `admin` | Bootstrap root-admin username. |
| `QUEUEFRAME_ADDR` | `127.0.0.1:8080` | HTTP listen address. |
| `QUEUEFRAME_BASE_URL` | `http://localhost:8080` | Canonical browser-facing origin used to derive the Kite callback. |
| `QUEUEFRAME_DB` | `./data/queueframe.db` | SQLite path. |
| `QUEUEFRAME_SECURE_COOKIES` | `false` | Must be `true` with HTTPS. |
| `QUEUEFRAME_EXPECTED_PUBLIC_IP` | empty | Displayed static outbound IPv4/IPv6; required for signup. |
| `QUEUEFRAME_TRUSTED_PROXY_CIDRS` | empty | Comma-separated proxy networks permitted to supply `X-Forwarded-For`; leave empty for direct/local access. The hardened `deploy/` stack derives this from its private network. |
| `QUEUEFRAME_DRY_RUN` | `true` | Prevent real placement writes when true. |
| `QUEUEFRAME_WARM_BEFORE` | `10s` | Bounded pre-target connection/profile warm-up lead; minimum `8s`. |
| `QUEUEFRAME_LATE_CUTOFF` | `5s` | Positive admission/recovery margin after each absolute batch target. Passed targets are never sent. |
| `QUEUEFRAME_WEB_SESSION_TTL` | `12h` | Positive local browser-session lifetime. |

Legacy `SELLATOPEN_*` names remain accepted as a migration bridge when the
corresponding `QUEUEFRAME_*` value is absent. New deployments should use only
the Queueframe names.

## Operations and verification

- `/healthz` is a minimal process liveness response, not proof of Kite, session,
  IP-whitelist, scheduler or exchange readiness.
- Keep gokiteconnect debug logging disabled; it can expose authorization
  headers.
- Containers inherit the host clock and cannot correct it. Verify the host's
  normal NTP service.
- Graceful shutdown drains HTTP and waits for the scheduler to finish bounded
  reconciliation/persistence before SQLite closes.
- The source includes race-tested timing, concurrency, no-retry, restart,
  identity, encryption, queue-revision and migration tests.

Routine status, stopped-volume backups, restore drills, safe updates and GCP
IAP recovery commands are in [docs/operations.md](docs/operations.md).

Run verification with:

```sh
go test ./...
go test -race ./...
go vet ./...
docker build --check .
```

Review Zerodha's current [Kite authentication lifecycle](https://kite.trade/docs/connect/v3/user/),
[order semantics](https://kite.trade/docs/connect/v3/orders/), and
[rate limits](https://kite.trade/docs/connect/v3/exceptions/) before enabling
live placement.

## Project status and trademarks

Queueframe is independent software and is not affiliated with, sponsored, or
endorsed by Zerodha. Zerodha, Kite, and Kite Connect are trademarks of their
respective owner. Queueframe is an order-submission tool, not investment advice,
and its scheduler targets do not guarantee broker acceptance, exchange receipt,
matching, or execution.

No open-source license has been selected yet. Until a `LICENSE` file is added,
the published source remains available for review but is not granted for reuse
or redistribution. The [release checklist](docs/releasing.md) records the
decision required before the repository is made public for self-hosting.
