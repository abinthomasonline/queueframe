# Security and household enrollment

Queueframe is a private household application, not a public trading service.
Publishing its HTTPS URL makes the login page discoverable; it does not make
new trader signup public.

## Invitation lifecycle

Only a local Queueframe administrator can authorize a first-time enrollment:

1. Sign in at `/admin/login`.
2. Open `/admin/invites` and create a single-use invitation that expires in
   1–30 days.
3. Send the complete link privately to one permitted household member.
4. When opened, Queueframe stores the invitation for at most 30 minutes in an
   `HttpOnly`, `SameSite=Strict` cookie and redirects to a clean URL so the
   token is not left in the visible address bar.
5. Submitting first-time signup atomically reserves that invitation for the
   Kite flow. A successful, identity-checked Kite callback consumes it.
6. The Kite flow reserves the invite for five minutes. A failed or abandoned
   flow releases it for a retry; a successful flow consumes it permanently.
7. An administrator can revoke any unused invite, including one currently in
   verification. Expired, revoked, or consumed invites cannot authorize a new
   trader.

The raw token is shown only in the creation response; Queueframe retains its
one-way hash and lifecycle metadata, not a recoverable copy of the link. The
invitation is nevertheless a bearer secret until it is consumed. Do not post
it in a group chat, issue, screenshot, shell history, analytics system, or
support request. Revoke it immediately if it goes to the wrong recipient.
Prefer a short expiry and create one link per person instead of forwarding one
link.

An invitation controls enrollment only. It does not establish an ongoing
Queueframe session, bypass Kite login, prove a family relationship, or permit
sharing an IP outside Zerodha's allowed group. Returning traders sign in with
their client ID and complete interactive Kite authentication.

## Zerodha boundary

Zerodha's current documentation says the shared-static-IP exception covers a
spouse, dependent children, and dependent parents through one developer account
with separate apps and client IDs. It is not a friends, colleagues, customers,
or general multi-tenant exception. Each invite recipient must still accept the
instance's household/static-IP declaration. Review [Zerodha's current static-IP
guidance](https://support.zerodha.com/category/trading-and-markets/general-kite/kite-api/articles/static-ip)
before creating an invitation.

Use one Kite Connect app per connected client ID and register the callback
shown by the instance exactly. Queueframe verifies that the client ID and API
key returned through Kite match the pending enrollment. Kite's documentation
also warns that API secrets and access tokens must not be exposed in client-side
applications; see the [Kite login flow](https://kite.trade/docs/connect/v3/user/).

## Secrets and trust

Queueframe encrypts Kite credentials and access tokens at rest with
`QUEUEFRAME_MASTER_KEY` and does not return them to browser JavaScript. This
protects a database copied without the key; it does not protect against a
compromised running host or an operator who has both the database and key.

Treat all of these as secrets:

- `deploy/.env`, especially the master key, bootstrap administrator password,
  and any Tailscale enrollment credential;
- SQLite data and every backup;
- unused invitation URLs;
- Kite API secrets and access tokens; and
- Caddy and Tailscale state volumes.

Keep `deploy/.env` at mode `600`, restrict SSH/IAP and cloud-console access,
encrypt off-host backups, and never commit a populated environment file. Losing
the master key makes encrypted credentials in the database unrecoverable;
leaking it together with the database exposes them.

After the first administrator login and running-health check succeed, use
`./deploy/seal.sh`. The command refuses an unhealthy deployment, atomically
removes the bootstrap password and any one-off Tailscale auth key from
`deploy/.env`, force-recreates the selected stack, and verifies that the new
container metadata no longer contains either value. The master key and all
persistent state remain intact. The exact procedure is in
[operations.md](operations.md#post-bootstrap-secret-sealing).

The bootstrap administrator is a separate local identity, not a Zerodha user.
Use a unique long password. Create additional administrators only for people
who should be able to inspect every trader, order, dispatch, and invitation.
Administrator access is sufficient to delete local traders and change runtime
settings, so it should not be shared casually.

## Network exposure

For domain and public-IP deployments:

- expose only Caddy on TCP 80 and 443;
- keep Queueframe's `8080` port private to the container network;
- use a private administrative path such as AWS Systems Manager Session
  Manager or Google Cloud IAP instead of public SSH; and
- keep the public address reserved, because changing it affects both HTTPS and
  the Kite egress whitelist.

The deployment stack gives its bridge a dedicated CIDR and configures that
exact range plus loopback as `QUEUEFRAME_TRUSTED_PROXY_CIDRS`. Caddy replaces
client-supplied forwarding headers with the socket client address; Queueframe
then selects the nearest untrusted hop for per-client authentication limits.
Do not broaden the trusted ranges to a host LAN or arbitrary private networks,
and never publish Queueframe's port 8080 around the proxy boundary.

For Tailscale deployments, expose no public web port. Tailscale Serve remains
subject to tailnet access rules. Every family browser must be part of the
tailnet; do not substitute Tailscale Funnel, which deliberately makes a service
public. See [Tailscale Serve's access model](https://tailscale.com/docs/features/tailscale-serve).

The domain or `*.ts.net` name is an inbound identity. Always configure
`QUEUEFRAME_EXPECTED_PUBLIC_IP` with the address outbound Kite order requests
actually use. Queueframe displays that operator-supplied value but deliberately
does not rely on an external “what is my IP” service as proof of whitelisting.

## HTTPS and browser protections

Set `QUEUEFRAME_SECURE_COOKIES=true` whenever the public base URL is HTTPS.
Queueframe rejects remote plain-HTTP base URLs, checks same-origin form
submissions, uses `HttpOnly` cookies, and sends restrictive browser security
headers. Those controls depend on the public origin being configured exactly;
do not add a path or silently switch between hostnames.

Caddy's certificate data must remain writable and persistent. Domain
certificates renew automatically under Caddy's normal HTTPS automation.
Numeric-IP certificates are valid for only 160 hours and use Let's Encrypt's
short-lived profile, so monitor renewal and keep Caddy running. See [Caddy's
storage warning](https://caddyserver.com/docs/conventions#data-directory) and
[Let's Encrypt's IP-certificate policy](https://letsencrypt.org/2026/01/15/6day-and-ip-general-availability.html).

## Operational safeguards

- Leave dry-run enabled until the complete family workflow has been rehearsed.
- Do not enable Kite client debug logging; authorization headers can contain
  credentials.
- Do not update, restart, back up, or resize the host during preparation or
  dispatch. Use the graceful Compose commands in [operations.md](operations.md).
- Review Queueframe's dispatch history and Kite's own order book. A local
  `ACKNOWLEDGED` outcome is not proof of an exchange fill.
- Test restores. An unread or incomplete backup is not a recovery plan.
- Patch the host, Docker, Caddy, and Queueframe, but take a verified backup
  before changing versions.
- Build releases with the pinned, currently supported Go patch release. Review
  Go and direct-dependency security updates before each release; do not keep an
  end-of-life compiler merely because the source still compiles with it.

## Incident response

If an invitation leaks, revoke it from `/admin/invites` and create a new one.
If a browser session or administrator password may be compromised, change the
administrator password and have traders sign in to Kite again as appropriate.

If the database and master key may both have leaked, assume stored Kite API
secrets and current tokens are exposed: stop Queueframe, rotate each Kite app's
credentials through Zerodha, invalidate affected sessions, and preserve
relevant audit evidence. Queueframe does not document an in-place master-key
rotation procedure; recover onto a clean database with a new key and re-enroll
permitted traders rather than improvising a partial re-encryption. Do not
simply delete logs or the VM before understanding the incident.

If the static public IP changes unexpectedly, keep live placement disabled.
Verify the new egress address, HTTPS origin and callback first, then update Kite
within its whitelist-change rules. Never work around an IP mismatch by routing
orders through an unreviewed third-party proxy.
