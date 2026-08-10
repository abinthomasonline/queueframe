# Operations

These procedures assume the production files in `deploy/` and the generated
`deploy/.env`. Commands use the Compose project name written by `setup.sh`.
Never run a maintenance operation during Queueframe's preparation, dispatch,
or result-persistence window.

## Routine checks

Run the deployment-aware checks and inspect the active services:

```sh
./deploy/doctor.sh --running
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml ps
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml logs --tail=200 queueframe
```

For a domain deployment, inspect Caddy with:

```sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml logs --tail=200 caddy-domain
```

Use `caddy-public-ip` instead for the public-IP profile, or `tailscale` for the
private profile. Do not enable verbose Kite client logging; authorization
headers can contain secrets.

Also verify the host clock periodically:

```sh
timedatectl status
```

Containers inherit the host clock. `/healthz` is process liveness only; it does
not prove Kite login, egress whitelisting, dispatch readiness, or exchange
connectivity.

## What must be backed up

A recoverable deployment needs all of the following:

- `deploy/.env`, containing the master key and immutable public-origin values;
- the complete Queueframe `/data` volume, including the SQLite database;
- the source/release version and deployment configuration used with that data;
- Caddy `/data` and `/config` for domain or public-IP mode; and
- `/var/lib/tailscale` for Tailscale mode.

The generated Compose project normally creates these Docker volumes:

| Data | Compose key | Default Docker volume name |
| --- | --- | --- |
| SQLite | `queueframe-data` | `queueframe_queueframe-data` |
| Caddy certificates/keys | `caddy-data` | `queueframe_caddy-data` |
| Caddy state | `caddy-config` | `queueframe_caddy-config` |
| Tailscale identity/state | `tailscale-state` | `queueframe_tailscale-state` |

Do not blindly rely on those expanded names. Confirm the project and labels on
the host first:

```sh
docker volume ls --filter label=com.docker.compose.project=queueframe
docker volume inspect queueframe_queueframe-data
```

Caddy warns that its data directory contains certificates, private keys and
other non-ephemeral state. Docker likewise treats volumes as the persistence
unit for backup and migration. See [Caddy's data-directory
documentation](https://caddyserver.com/docs/conventions#data-directory) and
[Docker's volume backup guidance](https://docs.docker.com/engine/storage/volumes/#back-up-restore-or-migrate-data-volumes).

## Consistent stopped backup

Choose a quiet time. The stop honors Queueframe's 90-second grace period, which
lets the scheduler finish bounded persistence and closes SQLite before it is
copied.

First record the running images, create a private destination, and stop the
whole stack:

```sh
QF_OPS_STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
QF_OPS_BACKUP_DIR="$PWD/queueframe-backup-$QF_OPS_STAMP"
install -d -m 700 "$QF_OPS_BACKUP_DIR"

docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml images >"$QF_OPS_BACKUP_DIR/images.txt"

docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml stop
```

Copy the app data and secret environment from the stopped container:

```sh
QF_OPS_APP_ID="$(docker compose --project-directory deploy \
  --env-file deploy/.env -f deploy/compose.yaml ps -aq queueframe)"
test -n "$QF_OPS_APP_ID"
install -d -m 700 "$QF_OPS_BACKUP_DIR/queueframe-data"
docker cp "$QF_OPS_APP_ID:/data/." "$QF_OPS_BACKUP_DIR/queueframe-data/"
install -m 600 deploy/.env "$QF_OPS_BACKUP_DIR/deploy.env"
```

For `domain`, also copy Caddy's two state directories:

```sh
QF_OPS_EDGE_ID="$(docker compose --project-directory deploy \
  --env-file deploy/.env -f deploy/compose.yaml ps -aq caddy-domain)"
test -n "$QF_OPS_EDGE_ID"
install -d -m 700 "$QF_OPS_BACKUP_DIR/caddy-data" \
  "$QF_OPS_BACKUP_DIR/caddy-config"
docker cp "$QF_OPS_EDGE_ID:/data/." "$QF_OPS_BACKUP_DIR/caddy-data/"
docker cp "$QF_OPS_EDGE_ID:/config/." "$QF_OPS_BACKUP_DIR/caddy-config/"
```

For `public-ip`, use the same commands with service `caddy-public-ip`. For
`tailscale`, copy its persistent state instead:

```sh
QF_OPS_EDGE_ID="$(docker compose --project-directory deploy \
  --env-file deploy/.env -f deploy/compose.yaml ps -aq tailscale)"
test -n "$QF_OPS_EDGE_ID"
install -d -m 700 "$QF_OPS_BACKUP_DIR/tailscale-state"
docker cp "$QF_OPS_EDGE_ID:/var/lib/tailscale/." \
  "$QF_OPS_BACKUP_DIR/tailscale-state/"
```

Restart promptly and verify before packaging the backup:

```sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml start
./deploy/doctor.sh --running

tar -C "$(dirname "$QF_OPS_BACKUP_DIR")" -czf "$QF_OPS_BACKUP_DIR.tar.gz" \
  "$(basename "$QF_OPS_BACKUP_DIR")"
sha256sum "$QF_OPS_BACKUP_DIR.tar.gz" \
  >"$QF_OPS_BACKUP_DIR.tar.gz.sha256"
```

The `.tar.gz` produced above is **not encrypted**. Encrypt it with the
operator's established backup tool before transfer, then move the encrypted
archive and checksum to off-host storage. The archive contains live trading
credentials; ordinary cloud-object privacy settings are not a substitute for
client-side encryption and access control. Retention should include
at least one backup from before every application update and a recent routine
backup. After verifying the protected copy, remove the unencrypted staging
directory and archive from the server using an explicit, rechecked path and the
host's approved secure-cleanup procedure.

A stopped cloud-disk snapshot can be a useful second layer, but it is not a
replacement for an export that has been restored and tested. Do not take a
crash-consistency shortcut while Queueframe is writing its dispatch result.

## Restore drill

Test restores on an isolated host before depending on them. The steps below
assume a clean Docker project with empty volumes; they deliberately do not
erase or merge an existing deployment. If `docker volume ls` shows volumes for
the target Compose project, stop and preserve them under a different project
name before proceeding.

1. Verify the archive checksum and unpack it into a directory with mode `700`.
2. Check out the source/release recorded by the backup.
3. Restore the secret environment before creating containers:

```sh
install -m 600 /path/to/unpacked-backup/deploy.env deploy/.env
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml create --build
```

4. Copy Queueframe's stopped data into the newly created container and repair
   ownership for the non-root Queueframe user:

```sh
QF_RESTORE_APP_ID="$(docker compose --project-directory deploy \
  --env-file deploy/.env -f deploy/compose.yaml ps -aq queueframe)"
docker cp /path/to/unpacked-backup/queueframe-data/. \
  "$QF_RESTORE_APP_ID:/data/"
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml run --rm --no-deps --user 0 --entrypoint sh \
  queueframe -c 'chown -R 10001:10001 /data'
```

5. For domain mode, copy `caddy-data/.` to `/data/` and `caddy-config/.` to
   `/config/` in the stopped `caddy-domain` container. Use `caddy-public-ip`
   for that profile. For Tailscale mode, copy `tailscale-state/.` to
   `/var/lib/tailscale/` in the stopped `tailscale` container.
6. Start the restored stack and verify it:

```sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml start
./deploy/doctor.sh --running
```

Confirm administrator login, trader history, current settings, invitation
history, HTTPS, and the configured callback. Keep dry-run enabled during a
restore drill. Do not point a second test instance with restored credentials at
live placement or run it concurrently with the production database.

Never restore the SQLite database with a newly generated master key. Never
downgrade an already migrated database by starting an older binary against it;
restore the matching pre-update database and application together.

## Safe update

Use a release tag or reviewed commit, not an unreviewed moving branch. Schedule
a maintenance window, take a stopped backup, and then run the repository's
checks:

```sh
./deploy/test.sh
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml build --pull queueframe
docker compose --project-directory deploy --env-file deploy/.env \
  -f deploy/compose.yaml up -d
./deploy/doctor.sh --running
```

Review `deploy/.env.example` and release notes for new required values, but do
not replace `deploy/.env` or regenerate its master key. `setup.sh` refuses to
overwrite the file for this reason. Updating
`QUEUEFRAME_ADMIN_PASSWORD` after the database has been bootstrapped does not
change the stored administrator password; use the administrator security page.

If verification fails after a schema migration, restore the complete matching
backup. Merely running the previous image against the newer database is not a
safe rollback.

## Post-bootstrap secret sealing

`QUEUEFRAME_ADMIN_PASSWORD` is needed only while an empty database creates its
first administrator. The Tailscale profile needs its one-off, pre-authorized
`tskey-*` only for the initial join. After the first administrator sign-in
succeeds, verify the running deployment and seal those bootstrap fields:

```sh
./deploy/doctor.sh --running
./deploy/seal.sh
```

`seal.sh` refuses to change the file unless Queueframe and the selected ingress
are running and the ingress can reach Queueframe's health endpoint. In
Tailscale mode it also requires the persisted node to be connected, its actual
DNS name to match the configured origin, and Serve to be actively proxying
Queueframe. It then atomically removes only the exact
`QUEUEFRAME_ADMIN_PASSWORD` and `TAILSCALE_AUTHKEY` fields from `deploy/.env`,
preserves its owner-only permissions, verifies that the master key remains,
force-recreates the selected stack, and checks the replacement containers for
both health and empty bootstrap environment values. This recreation is what
removes secrets from Docker's stored container metadata; editing `.env` alone
does not. The command is safe to run again and repairs contaminated containers
left by an older deployment script.

Changing or re-adding the bootstrap environment value later does not change an
existing administrator's stored password; use `/admin/security`. If restoring
a genuinely empty database, deliberately add a new temporary
`QUEUEFRAME_ADMIN_PASSWORD` of at least 12 characters before starting it, then
repeat the sign-in, health check, and seal process.

If the `tailscale-state` volume is lost, create a new one-off key, deliberately
add `TAILSCALE_AUTHKEY` back to the mode-`600` file, authorize the replacement
node, and repeat the running check and seal. Revoking or expiring the original
enrollment key does not remove a node whose identity remains in the persistent
volume.

## Cloud administration and recovery

On AWS, use Systems Manager Session Manager so the instance does not need a
public SSH rule:

```sh
aws ssm start-session \
  --target i-REPLACE_ME \
  --region ap-south-1 \
  --profile queueframe-aws
```

See the [AWS deployment guide](aws.md#session-manager) and AWS's [Session
Manager documentation](https://docs.aws.amazon.com/systems-manager/latest/userguide/session-manager.html).

On Google Cloud, use IAP for administration even though the Queueframe VM has
a public web address:

```sh
gcloud compute ssh queueframe \
  --project=YOUR_PROJECT_ID \
  --zone=YOUR_ZONE \
  --tunnel-through-iap
```

Google documents that IAP wraps the SSH connection in HTTPS and applies IAM
authorization; the firewall should allow TCP 22 only from IAP's documented
range. See [IAP SSH](https://docs.cloud.google.com/compute/docs/connect/ssh-using-iap).

If the production ingress is unavailable, use SSH to inspect Compose state,
DNS, clock synchronization and logs first. The loopback root
`docker-compose.yml` and localhost tunnel in [deployment.md](deployment.md#ssh-tunnel-recovery)
are a separate recovery configuration. They do not automatically mount the
production Compose volume, so do not expect production users or orders to
appear there unless a deliberate, stopped restore has been performed.

Before replacing a VM or static IP, make a verified backup. An AWS Elastic IP
or reserved GCP address can be reassigned to a replacement VM; releasing it can
break HTTPS, the configured callback, and the Kite whitelist together.
