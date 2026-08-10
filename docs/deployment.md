# Deployment

Queueframe supports three HTTPS access modes. All three run one Queueframe
process with a local SQLite database and require a stable public **egress** IP
for Kite order placement. The way a browser reaches Queueframe does not decide
which address Kite sees on outbound requests.

| Mode | Browser access | Domain required | Public web ports | Use when |
| --- | --- | --- | --- | --- |
| Domain + Caddy | Public, trusted HTTPS | Yes | TCP 80/443, UDP 443 | Recommended for a household |
| Tailscale Serve | Private, trusted HTTPS inside one tailnet | No | None | Recommended without a domain |
| Public IP certificate | Public, trusted HTTPS at a numeric IP | No | TCP 80/443, UDP 443 | Advanced fallback only |

An SSH tunnel is also available as a recovery path. It is intentionally not a
deployment profile because every user must keep a tunnel running.

## Invariants for every mode

- Use a stable public address for outbound traffic and put that exact address
  in `QUEUEFRAME_EXPECTED_PUBLIC_IP`. A directly attached AWS Elastic IP or a
  reserved Google Cloud external address provides this identity on the
  single-VM designs in the cloud guides. A reverse proxy, DNS record, or
  Tailscale address does not replace the egress requirement. See [AWS internet
  gateway NAT](https://docs.aws.amazon.com/vpc/latest/userguide/VPC_Internet_Gateway.html),
  [Google Cloud Public NAT](https://docs.cloud.google.com/nat/docs/public-nat),
  and [Zerodha's current static-IP
  rules](https://support.zerodha.com/category/trading-and-markets/general-kite/kite-api/articles/static-ip).
- Run exactly one Queueframe process against the database. Keep SQLite on a
  local persistent filesystem, not NFS, SMB, or a shared container volume.
- Keep `QUEUEFRAME_DRY_RUN=true` through onboarding and a complete rehearsal.
- Use HTTPS for every non-loopback URL. `QUEUEFRAME_BASE_URL` is an origin only:
  no path, query, or fragment.
- Register the callback shown by Queueframe exactly. It is always
  `${QUEUEFRAME_BASE_URL}/auth/kite/callback`.
- Keep the generated master key with the database backups. Credentials in a
  restored database cannot be decrypted with a new key.
- New trader enrollment is invitation-only. An administrator creates a
  short-lived, single-use link at `/admin/invites`; an invite does not relax
  Zerodha's household or static-IP rules.

Zerodha currently permits one developer account to share its static IP only
with a spouse, dependent children, and dependent parents, using separate apps
and client IDs. It also says a whitelist address can be changed only once per
calendar week. Confirm the address before saving it in the developer console.

## Common setup

From a Queueframe checkout on the server:

```sh
./deploy/setup.sh
./deploy/doctor.sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml up --build -d
./deploy/doctor.sh --running
```

`setup.sh` asks for an access mode and its required values, generates the
master key, writes `deploy/.env` with mode `600`, and leaves dry-run enabled. It
refuses to overwrite an existing environment file. Back up that file instead
of deleting it when changing or rebuilding a host.

Useful inspection commands are:

```sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml ps
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml logs --tail=200 queueframe
```

The deployment Compose file does not publish Queueframe's port `8080` on the
host. In domain and public-IP modes, Caddy is the only public web entry point.
Its `/data` and `/config` directories are persistent; Caddy's documentation
specifically warns that its data directory contains certificates and keys and
must not be treated as a cache. See [Caddy's official Docker image
notes](https://hub.docker.com/_/caddy).

## Mode 1: domain + Caddy

This is the default for a public household instance.

1. Reserve the server's public egress address before changing DNS or the Kite
   whitelist.
2. Create an `A` record from the chosen hostname to that IPv4 address. Add an
   `AAAA` record only when IPv6 ingress is deliberately configured and tested.
3. Open inbound TCP ports `80` and `443`; do not open `8080`.
4. Run `./deploy/setup.sh`, select `domain`, and enter the full hostname without
   a scheme or path.
5. Run the common start and verification commands above.

For example, a public origin could be:

```text
https://queueframe.example.com
```

and its exact Kite callback is:

```text
https://queueframe.example.com/auth/kite/callback
```

Caddy obtains and renews a browser-trusted certificate and redirects HTTP to
HTTPS when DNS points to the server, ports 80 and 443 are reachable, and its
data directory is writable and persistent. Those are Caddy's documented
[automatic HTTPS requirements](https://caddyserver.com/docs/automatic-https).

If Cloudflare hosts the zone, use an `A` record with **DNS only** while bringing
up and diagnosing the origin. In DNS-only mode Cloudflare returns the actual
origin address rather than proxying HTTP traffic; see [Cloudflare's proxy-status
documentation](https://developers.cloudflare.com/dns/proxy-status/). The
shipped doctor expects DNS to include the configured ingress address. Treat a
later switch to Cloudflare's proxy as a separate, deliberately tested topology:
it adds another TLS and request-routing layer, makes the direct-DNS check fail,
and does not change Queueframe's configured egress address.

## Mode 2: Tailscale Serve

Use this when the family accepts installing Tailscale and the service should
not be public.

1. The server still needs a stable outbound public IP for Kite. It does not
   need public inbound ports 80, 443, or 8080.
2. Create or choose a tailnet, invite only permitted household members, enable
   MagicDNS and HTTPS, and decide on the server's stable machine name.
3. Create a one-off, pre-authorized `tskey-*` for the server. It is an
   enrollment credential, not a key to share with family browsers.
4. Run `./deploy/setup.sh`, select `tailscale`, and supply the full `*.ts.net`
   name, machine name, and one-off key. The generated Compose profile persists
   Tailscale state and applies `deploy/tailscale/serve.json`.
5. Start the stack, inspect the Tailscale service logs, then run
   `./deploy/doctor.sh --running` again.
6. After the persisted node and first administrator sign-in are confirmed, run
   `./deploy/seal.sh` as described in
   [operations.md](operations.md#post-bootstrap-secret-sealing). It verifies
   the active Serve proxy before removing the one-off key.
7. Confirm every household device can reach the HTTPS URL while connected to
   the tailnet.

A typical origin and callback are:

```text
https://queueframe.example-tailnet.ts.net
https://queueframe.example-tailnet.ts.net/auth/kite/callback
```

The browser completing Kite login must be connected to the tailnet so it can
reach the callback. Tailscale Serve is private to the tailnet and its access
rules still apply; Funnel is a different feature that makes a service public
and is not used by this profile. Serve requires HTTPS certificates and
MagicDNS. Enabling HTTPS publishes the machine and tailnet DNS names in public
certificate-transparency logs. Review [Tailscale Serve](https://tailscale.com/docs/features/tailscale-serve)
and [Tailscale's HTTPS disclosure](https://tailscale.com/docs/how-to/set-up-https-certificates)
before enabling it. Tailscale recommends a one-off key when authenticating one
device and automatically revokes it after use; see [Tailscale auth
keys](https://tailscale.com/docs/features/access-control/auth-keys).

## Mode 3: public numeric IP certificate

This mode is for a host with a reserved public IP but no domain and where every
family browser needs direct public access. Prefer a domain or Tailscale when
either is practical.

1. Reserve the IP before setup. Losing it changes the URL, Kite callback,
   expected egress setting, whitelist, and certificate identity at once.
2. Open inbound TCP ports `80` and `443`; do not open `8080`.
3. Run `./deploy/setup.sh`, select `public-ip`, and enter the numeric address.
   IPv6 origins use URL brackets, for example `https://[2001:db8::10]`.
4. Start the stack and keep the Caddy data volume intact.
5. Monitor `./deploy/doctor.sh --running` and Caddy's logs. Certificate renewal
   failure becomes an outage quickly in this mode.

For IPv4 `203.0.113.10`, the callback shape is:

```text
https://203.0.113.10/auth/kite/callback
```

Let's Encrypt now issues IPv4 and IPv6 certificates, but IP certificates must
use its `shortlived` profile and are valid for only 160 hours. The supplied
Compose file pins Caddy `2.11.4-alpine` by image digest and
`deploy/caddy/Caddyfile.public-ip` explicitly selects that profile; continuous
automated renewal is therefore mandatory. Do not downgrade Caddy without
retesting IP issuance and renewal. See [Let's Encrypt's IP
certificate announcement](https://letsencrypt.org/2026/01/15/6day-and-ip-general-availability.html)
and [Caddy's ACME profile syntax](https://caddyserver.com/docs/caddyfile/directives/tls).

## SSH-tunnel recovery

The root `docker-compose.yml` remains a loopback-only development and recovery
configuration. It publishes Queueframe at `127.0.0.1:8080`, without Caddy or a
Tailscale sidecar.

On the server:

```sh
docker compose -f docker-compose.yml up --build -d
```

On AWS, prefer Systems Manager Session Manager and keep port 22 closed:

```sh
aws ssm start-session \
  --target i-REPLACE_ME \
  --document-name AWS-StartPortForwardingSession \
  --parameters 'portNumber=["8080"],localPortNumber=["8080"]' \
  --region ap-south-1 \
  --profile queueframe-aws
```

The workstation needs the Session Manager plugin. On Google Cloud, use IAP:

```sh
gcloud compute ssh queueframe \
  --project=YOUR_PROJECT_ID \
  --zone=YOUR_ZONE \
  --tunnel-through-iap \
  -- -N -L 8080:127.0.0.1:8080
```

Then browse to `http://localhost:8080`. This fallback requires
`QUEUEFRAME_BASE_URL=http://localhost:8080`,
`QUEUEFRAME_SECURE_COOKIES=false`, and this exact callback:

```text
http://localhost:8080/auth/kite/callback
```

Do not use a remote, non-loopback HTTP URL. The tunnel is encrypted, but the
browser URL remains localhost and every user needs their own authorized tunnel.

## After the first start

1. Open `/admin/login` and sign in with the bootstrap administrator account.
2. After that login and `./deploy/doctor.sh --running` are verified, run
   `./deploy/seal.sh`. It atomically removes the bootstrap administrator
   password and any Tailscale enrollment key while retaining the master key.
3. Change the administrator password from the security page when needed.
4. Open `/admin/invites`, create a short-lived invite, and send it privately to
   one permitted trader.
5. The trader opens the link, enters their own client ID, API key, and API
   secret, accepts the household declaration, and completes Kite login. A
   successful Kite callback consumes the invite.
6. Revoke any unused invite that was sent to the wrong person or exposed.
7. Finish a full dry-run rehearsal before considering
   `QUEUEFRAME_DRY_RUN=false`.

See [security.md](security.md) for the invitation threat model and
[operations.md](operations.md) before enabling live placement.
