# Self-hosting Queueframe

This directory provides one hardened Compose stack with three HTTPS access
modes. Queueframe itself has no published host port: only the selected Caddy
or Tailscale ingress can reach it.

| Mode | Browser access | Public ports | Use when |
| --- | --- | --- | --- |
| `domain` | `https://queueframe.example.com` | TCP 80/443, UDP 443 | Recommended family deployment |
| `tailscale` | `https://queueframe.<tailnet>.ts.net` | None | Private family deployment without a domain |
| `public-ip` | `https://203.0.113.10` | TCP 80/443, UDP 443 | Advanced public deployment without a domain |

All modes still need a stable public **outbound** address for Zerodha's IP
whitelist. An ingress address and outbound address are often the same on a
small VPS, but the setup keeps them distinct for hosts that use NAT.

For a cloud VM, use the recommended [AWS Mumbai guide](../docs/aws.md). The
[Google Cloud Mumbai guide](../docs/gcp.md) is a supported alternative.

## Set up

Install Docker Engine with the Compose v2 plugin and OpenSSL on a Linux host.
Keep its normal NTP service enabled. Then run from the repository root:

```sh
./deploy/setup.sh
./deploy/doctor.sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml up --build -d
./deploy/doctor.sh --running
./deploy/seal.sh
```

Setup generates a standard-base64 32-byte master key, asks for the initial
administrator password without echoing it, keeps dry-run enabled, writes
`deploy/.env` with mode `600`, and refuses to overwrite an existing file. The
doctor never sources the dotenv file as shell code and never prints a secret.
Seal refuses to run until Queueframe and the selected ingress are healthy; in
Tailscale mode it also verifies a `Running` backend, healthy tailnet address,
active Serve proxy, and an actual joined-node DNS name exactly matching the
configured URL. It then atomically removes only the bootstrap administrator
password and Tailscale auth-key fields, verifies that the master key is
unchanged, force-recreates the active containers, and inspects their new
environment to ensure those values are empty. If recreation fails, it retains
the master key and all persistent state and prints the exact recovery command.
The command is safe to rerun. It also detects and repairs stale plaintext in
stopped or running Docker container metadata when an older script or a manual
edit already removed the corresponding dotenv fields.

The generated `QUEUEFRAME_DOCKER_SUBNET` is a dedicated private `/24` used by
both Compose IPAM and Queueframe's trusted-proxy allowlist. Together with
loopback, this lets per-client rate limiting safely use Caddy/Tailscale's
sanitized `X-Forwarded-For` value without trusting the host LAN or arbitrary
private networks. If that subnet overlaps an existing Docker/VPN network,
choose another aligned private `/24` through `/29` and rerun the doctor before
starting the stack.

The Kite redirect URL is always the browser URL plus
`/auth/kite/callback`. Register that exact URL in every household member's Kite
app. Keep `QUEUEFRAME_DRY_RUN=true` through onboarding and at least one complete
rehearsal.

### Domain (recommended)

Create an A or AAAA record for the chosen hostname pointing directly to the
configured ingress IP. Start with any CDN/proxy feature disabled so the doctor
can verify the record and Caddy can complete ACME validation. Allow inbound TCP
80/443 and UDP 443. Caddy obtains and renews the normal public certificate and
stores its state in persistent volumes.

### Tailscale (no domain, private)

Enable HTTPS in the tailnet, choose the full `*.ts.net` name, and create a
one-off, pre-authorized auth key. Family members must join the same tailnet.
Tailscale Serve terminates trusted HTTPS and proxies only within the shared
container network namespace; Funnel is explicitly disabled and no host port is
published. `doctor.sh --running` rejects typos, wrong tailnets, and hostname
collision suffixes by comparing the joined node's actual DNS name to the base
URL. `seal.sh` removes the one-off auth key only after those checks pass.

### Public static IP (no domain, advanced)

The public ingress address must remain assigned to this host. The Caddyfile
explicitly selects Let's Encrypt's `shortlived` ACME profile for its public IP
certificate. Those certificates are valid for about 160 hours, so Caddy must
remain running, ports 80/443 must remain reachable, and both Caddy volumes must
be preserved. IPv4 and bracketed IPv6 browser URLs are supported.

## Operations

Use the same Compose prefix for every command:

```sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml ps
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml logs -f --tail=200
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml down
```

`down` preserves named volumes. Never use `down -v` unless the permanent loss
of the database, certificates, and Tailscale identity is intentional.

After the first successful administrator login, change the password in the
administrator UI. `seal.sh` removes the no-longer-needed bootstrap value from
`deploy/.env`. The master key must remain stable: losing it makes stored broker
credentials unrecoverable.

The volume keys are `queueframe-data`, `caddy-data`, `caddy-config`, and
`tailscale-state`. With the generated project name their Docker names are
`queueframe_queueframe-data`, `queueframe_caddy-data`,
`queueframe_caddy-config`, and `queueframe_tailscale-state`. Stop Queueframe
before taking a raw volume archive so the SQLite database and WAL are coherent;
back up the archive and `deploy/.env` separately with encryption and access
controls. Restore only while the stack is stopped.

For a source update:

```sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml build --pull queueframe
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml up -d
```

The Compose file also supports a prebuilt image. Set `QUEUEFRAME_IMAGE` to an
immutable release reference (preferably including a digest), pull it, and
start without building:

```sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml pull queueframe
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml up --no-build -d
```

Tagged Queueframe releases provide `amd64` and `arm64` images with an SBOM and
build provenance. After choosing a release, copy its immutable digest into
`QUEUEFRAME_IMAGE`, for example
`ghcr.io/abinthomasonline/queueframe:VERSION@sha256:DIGEST`. Official images
are publicly pullable without a registry login. Forks should replace the owner
and repository; private fork packages require authentication. The source-build
commands above remain the fallback when no suitable image is available.

Run deployment validation after changes:

```sh
./deploy/test.sh
./deploy/doctor.sh --running
```

## SSH-tunnel fallback

For one operator, the repository-root `docker-compose.yml` can remain bound to
`127.0.0.1:8080` on the server and be reached through an SSH local forward:

```sh
ssh -N -L 8080:127.0.0.1:8080 user@server
```

Use `http://localhost:8080` and
`http://localhost:8080/auth/kite/callback` in that mode. It is deliberately not
a deploy profile: every family member would need SSH access and a continuously
running tunnel, so domain or Tailscale mode is easier and safer for shared use.
