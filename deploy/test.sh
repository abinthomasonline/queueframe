#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/queueframe-deploy-test.XXXXXX")
DOMAIN_ENV=$TEST_DIR/domain.env
TAILSCALE_ENV=$TEST_DIR/tailscale.env
PUBLIC_IP_ENV=$TEST_DIR/public-ip.env
ROUNDTRIP_ENV=$TEST_DIR/roundtrip.env
SCRUB_ENV=$TEST_DIR/scrub.env
SCRUB_EXPECTED_ENV=$TEST_DIR/scrub-expected.env
REFUSAL_ENV=$TEST_DIR/refusal.env
SEALED_ENV=$TEST_DIR/already-sealed.env
FAKE_DOCKER=$TEST_DIR/docker
FAKE_DOCKER_STATE=$TEST_DIR/fake-docker-recreated
FAKE_DOCKER_LOG=$TEST_DIR/fake-docker.log
CADDY_IMAGE='caddy:2.11.4-alpine@sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648'

# shellcheck source=deploy/lib.sh
. "$SCRIPT_DIR/lib.sh"

cleanup() {
	for _qf_test_file in "$TEST_DIR"/*; do
		[ -e "$_qf_test_file" ] || continue
		rm -f -- "$_qf_test_file"
	done
	rmdir "$TEST_DIR" 2>/dev/null || true
}
trap cleanup EXIT HUP INT TERM

PASS_COUNT=0

ok() {
	PASS_COUNT=$((PASS_COUNT + 1))
	printf 'ok %d - %s\n' "$PASS_COUNT" "$1"
}

fail_test() {
	printf 'not ok - %s\n' "$1" >&2
	exit 1
}

assert_valid() {
	_description=$1
	shift
	"$@" || fail_test "$_description"
	ok "$_description"
}

assert_invalid() {
	_description=$1
	shift
	if "$@"; then
		fail_test "$_description"
	fi
	ok "$_description"
}

compose() {
	_env_file=$1
	shift
	docker compose --project-directory "$SCRIPT_DIR" --env-file "$_env_file" -f "$SCRIPT_DIR/compose.yaml" "$@"
}

assert_valid 'IPv4 validation accepts a routable shape' qf_valid_ip 203.0.113.10
assert_valid 'IPv6 validation accepts compressed addresses' qf_valid_ip 2001:db8::10
assert_valid 'IPv6 validation accepts the unspecified address' qf_valid_ip ::
assert_valid 'IPv6 validation accepts embedded IPv4' qf_valid_ip ::ffff:192.0.2.10
assert_invalid 'IP validation rejects an oversized IPv4 octet' qf_valid_ip 203.0.113.999
assert_invalid 'IP validation rejects repeated IPv6 compression' qf_valid_ip 2001::db8::10
assert_invalid 'IP validation rejects nine IPv6 groups' qf_valid_ip 1:2:3:4:5:6:7:8:9
assert_valid 'domain validation accepts a subdomain' qf_valid_domain queueframe.example.com
assert_invalid 'domain validation rejects URL syntax' qf_valid_domain https://queueframe.example.com
assert_invalid 'domain validation rejects underscores' qf_valid_domain queue_frame.example.com
assert_valid 'base URL validation accepts an IPv6 HTTPS origin' qf_valid_base_url 'https://[2001:db8::10]'
assert_invalid 'base URL validation rejects paths' qf_valid_base_url https://queueframe.example.com/path
assert_valid 'Docker subnet validation accepts an aligned private /24' qf_valid_private_docker_subnet 172.30.255.0/24
assert_valid 'Docker subnet validation accepts an aligned private /29' qf_valid_private_docker_subnet 10.23.42.8/29
assert_invalid 'Docker subnet validation rejects a public range' qf_valid_private_docker_subnet 203.0.113.0/24
assert_invalid 'Docker subnet validation rejects an unaligned range' qf_valid_private_docker_subnet 172.30.255.1/24
assert_invalid 'Docker subnet validation rejects an overly broad trust range' qf_valid_private_docker_subnet 172.30.0.0/16

TAILSCALE_STATUS_FIXTURE='{
  "BackendState": "Running",
  "Self": {
    "DNSName": "queueframe.example-tailnet.ts.net."
  },
  "Peer": {
    "peer": {"DNSName": "not-queueframe.example-tailnet.ts.net."}
  }
}'
[ "$(printf '%s\n' "$TAILSCALE_STATUS_FIXTURE" | qf_tailscale_backend_from_status)" = Running ] || fail_test 'Tailscale backend-state parser is wrong'
[ "$(printf '%s\n' "$TAILSCALE_STATUS_FIXTURE" | qf_tailscale_dns_from_status)" = queueframe.example-tailnet.ts.net ] || fail_test 'Tailscale self-DNS parser selected the wrong node'
ok 'Tailscale status parsing requires Running and selects exact Self.DNSName'

umask 077
: >"$ROUNDTRIP_ENV"
SPECIAL_VALUE="space dollar \$ slash \\ quote '"
qf_dotenv_write "$ROUNDTRIP_ENV" SPECIAL_VALUE "$SPECIAL_VALUE"
[ "$(qf_dotenv_get "$ROUNDTRIP_ENV" SPECIAL_VALUE)" = "$SPECIAL_VALUE" ] || fail_test 'dotenv values do not round-trip safely'
ok 'dotenv quoting round-trips spaces and shell metacharacters'

cat >"$SCRUB_ENV" <<'EOF'
KEEP_ONE='alpha'
QUEUEFRAME_MASTER_KEY='AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='
QUEUEFRAME_ADMIN_PASSWORD='bootstrap-password'
TAILSCALE_AUTHKEY='tskey-auth-test-not-real'
TAILSCALE_AUTHKEY_BACKUP='must-remain'
KEEP_TWO='omega'
EOF
cat >"$SCRUB_EXPECTED_ENV" <<'EOF'
KEEP_ONE='alpha'
QUEUEFRAME_MASTER_KEY='AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='
TAILSCALE_AUTHKEY_BACKUP='must-remain'
KEEP_TWO='omega'
EOF
chmod 600 "$SCRUB_ENV" "$SCRUB_EXPECTED_ENV"
qf_scrub_bootstrap_credentials "$SCRUB_ENV" || fail_test 'credential scrub helper failed'
cmp -s "$SCRUB_ENV" "$SCRUB_EXPECTED_ENV" || fail_test 'credential scrub changed fields beyond its allowlist'
[ "$(qf_file_mode "$SCRUB_ENV")" = 600 ] || fail_test 'credential scrub changed dotenv permissions'
SCRUB_CKSUM=$(cksum "$SCRUB_ENV")
qf_scrub_bootstrap_credentials "$SCRUB_ENV" || fail_test 'credential scrub was not idempotent'
[ "$(cksum "$SCRUB_ENV")" = "$SCRUB_CKSUM" ] || fail_test 'idempotent scrub changed the file'
ok 'credential scrub is exact-key-only, owner-only, master-key-safe, and idempotent'

TEST_PASSWORD='a-safe-test-password'
printf '%s\n' \
	'1' 'admin' "$TEST_PASSWORD" "$TEST_PASSWORD" '203.0.113.10' \
	'queueframe.example.com' '' 'admin@example.com' |
	QUEUEFRAME_DEPLOY_ENV_FILE="$DOMAIN_ENV" "$SCRIPT_DIR/setup.sh" >/dev/null
[ "$(qf_file_mode "$DOMAIN_ENV")" = 600 ] || fail_test 'setup did not set mode 600'
ok 'domain setup writes an owner-only dotenv file'
[ "$(qf_dotenv_get "$DOMAIN_ENV" QUEUEFRAME_BASE_URL)" = 'https://queueframe.example.com' ] || fail_test 'domain base URL is wrong'
[ "$(qf_dotenv_get "$DOMAIN_ENV" QUEUEFRAME_DRY_RUN)" = true ] || fail_test 'domain setup did not default to dry-run'
ok 'domain setup derives HTTPS and keeps dry-run enabled'
DOMAIN_CKSUM=$(cksum "$DOMAIN_ENV")
QUEUEFRAME_DEPLOY_ENV_FILE="$DOMAIN_ENV" "$SCRIPT_DIR/setup.sh" >/dev/null
[ "$(cksum "$DOMAIN_ENV")" = "$DOMAIN_CKSUM" ] || fail_test 'setup overwrote an existing dotenv file'
ok 'setup is idempotent and refuses to overwrite existing configuration'

awk '
	/^COMPOSE_PROJECT_NAME=/ { print "COMPOSE_PROJECT_NAME=queueframe-seal-refusal-test"; next }
	{ print }
' "$DOMAIN_ENV" >"$REFUSAL_ENV"
chmod 600 "$REFUSAL_ENV"
REFUSAL_CKSUM=$(cksum "$REFUSAL_ENV")
if QUEUEFRAME_DEPLOY_ENV_FILE="$REFUSAL_ENV" "$SCRIPT_DIR/seal.sh" >/dev/null 2>&1; then
	fail_test 'seal accepted a deployment whose services were not healthy'
fi
[ "$(cksum "$REFUSAL_ENV")" = "$REFUSAL_CKSUM" ] || fail_test 'failed health validation changed the dotenv file'
ok 'seal refuses unhealthy deployments without changing configuration'

cat >"$SEALED_ENV" <<'EOF'
COMPOSE_PROJECT_NAME='queueframe-seal-upgrade-test'
COMPOSE_PROFILES='domain'
QUEUEFRAME_DEPLOY_MODE='domain'
QUEUEFRAME_MASTER_KEY='AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='
EOF
chmod 600 "$SEALED_ENV"
cat >"$FAKE_DOCKER" <<'EOF'
#!/bin/sh
set -eu
printf '%s\n' "$*" >>"$QUEUEFRAME_FAKE_DOCKER_LOG"
if [ "$1" = info ]; then
	exit 0
fi
if [ "$1" = inspect ]; then
	case ${4:-} in
	queueframe-old) printf '%s\n' 'QUEUEFRAME_ADMIN_PASSWORD=not-real-stale-value' ;;
	queueframe-new) printf '%s\n' 'QUEUEFRAME_ADMIN_PASSWORD=' ;;
	*) exit 1 ;;
	esac
	exit 0
fi
case " $* " in
*' config --quiet '*) exit 0 ;;
*' ps --all -q queueframe '*)
	if [ -f "$QUEUEFRAME_FAKE_DOCKER_STATE" ]; then
		printf '%s\n' queueframe-new
	else
		printf '%s\n' queueframe-old
	fi
	;;
*' up --no-build --force-recreate -d '*) : >"$QUEUEFRAME_FAKE_DOCKER_STATE" ;;
*' ps --status running --services '*) printf '%s\n' queueframe caddy-domain ;;
*' exec -T caddy-domain wget '*) exit 0 ;;
*' ps --status running -q queueframe '*) printf '%s\n' queueframe-new ;;
*) exit 1 ;;
esac
EOF
chmod 700 "$FAKE_DOCKER"
SEALED_CKSUM=$(cksum "$SEALED_ENV")
if ! SEALED_OUTPUT=$(PATH="$TEST_DIR:$PATH" \
	QUEUEFRAME_FAKE_DOCKER_STATE="$FAKE_DOCKER_STATE" \
	QUEUEFRAME_FAKE_DOCKER_LOG="$FAKE_DOCKER_LOG" \
	QUEUEFRAME_DEPLOY_ENV_FILE="$SEALED_ENV" "$SCRIPT_DIR/seal.sh" 2>&1); then
	fail_test 'seal could not repair stale container metadata after a prior/manual dotenv scrub'
fi
[ -f "$FAKE_DOCKER_STATE" ] || fail_test 'seal did not force-recreate the already-scrubbed stack'
[ "$(cksum "$SEALED_ENV")" = "$SEALED_CKSUM" ] || fail_test 'metadata repair changed the already-sealed dotenv file'
grep -Fq 'inspect --format' "$FAKE_DOCKER_LOG" || fail_test 'metadata repair did not inspect old and fresh containers'
grep -Fq 'up --no-build --force-recreate -d' "$FAKE_DOCKER_LOG" || fail_test 'metadata repair did not force-recreate the stack'
case $SEALED_OUTPUT in
*not-real-stale-value*) fail_test 'seal printed a stale credential value' ;;
esac
ok 'seal repairs stale Docker Config.Env after a prior/manual dotenv scrub without exposing or rewriting secrets'

printf '%s\n' \
	'2' 'admin' "$TEST_PASSWORD" "$TEST_PASSWORD" '203.0.113.20' \
	'queueframe.example-tailnet.ts.net' '' 'tskey-auth-test-value-not-real' |
	QUEUEFRAME_DEPLOY_ENV_FILE="$TAILSCALE_ENV" "$SCRIPT_DIR/setup.sh" >/dev/null
[ "$(qf_dotenv_get "$TAILSCALE_ENV" QUEUEFRAME_BASE_URL)" = 'https://queueframe.example-tailnet.ts.net' ] || fail_test 'Tailscale base URL is wrong'
[ "$(qf_dotenv_get "$TAILSCALE_ENV" COMPOSE_PROFILES)" = tailscale ] || fail_test 'Tailscale profile is wrong'
ok 'Tailscale setup derives its private HTTPS origin and profile'

printf '%s\n' \
	'3' 'admin' "$TEST_PASSWORD" "$TEST_PASSWORD" '2001:db8::10' \
	'' 'admin@example.com' |
	QUEUEFRAME_DEPLOY_ENV_FILE="$PUBLIC_IP_ENV" "$SCRIPT_DIR/setup.sh" >/dev/null
[ "$(qf_dotenv_get "$PUBLIC_IP_ENV" QUEUEFRAME_BASE_URL)" = 'https://[2001:db8::10]' ] || fail_test 'public IPv6 base URL is wrong'
[ "$(qf_dotenv_get "$PUBLIC_IP_ENV" QUEUEFRAME_CADDY_IP_SITE)" = 'https://[2001:db8::10]' ] || fail_test 'Caddy IPv6 site is wrong'
ok 'public-IP setup brackets IPv6 browser and Caddy origins'

for _qf_test_env in "$DOMAIN_ENV" "$TAILSCALE_ENV" "$PUBLIC_IP_ENV"; do
	MASTER_KEY=$(qf_dotenv_get "$_qf_test_env" QUEUEFRAME_MASTER_KEY)
	MASTER_BYTES=$(printf '%s' "$MASTER_KEY" | openssl base64 -d -A 2>/dev/null | wc -c | tr -d '[:space:]')
	[ "$MASTER_BYTES" = 32 ] || fail_test 'setup generated an invalid master key'
	[ "$(qf_dotenv_get "$_qf_test_env" QUEUEFRAME_DOCKER_SUBNET)" = 172.30.255.0/24 ] || fail_test 'setup did not generate the dedicated Docker subnet'
	docker compose --project-directory "$SCRIPT_DIR" --env-file "$_qf_test_env" -f "$SCRIPT_DIR/compose.yaml" config --quiet >/dev/null || fail_test 'Compose configuration does not render'
done
ok 'every setup mode generates a 32-byte key, dedicated subnet, and valid Compose configuration'

[ "$(compose "$DOMAIN_ENV" config --services)" = "queueframe
caddy-domain" ] || fail_test 'domain profile enables the wrong services'
[ "$(compose "$TAILSCALE_ENV" config --services)" = "queueframe
tailscale" ] || fail_test 'Tailscale profile enables the wrong services'
[ "$(compose "$PUBLIC_IP_ENV" config --services)" = "queueframe
caddy-public-ip" ] || fail_test 'public-IP profile enables the wrong services'
ok 'each profile enables Queueframe plus exactly one ingress service'

if command -v jq >/dev/null 2>&1; then
	compose "$DOMAIN_ENV" config --format json >"$TEST_DIR/compose.json"
	compose "$TAILSCALE_ENV" config --format json >"$TEST_DIR/compose-tailscale.json"
	jq -e '
		(.services.queueframe.ports // [] | length) == 0 and
		.services.queueframe.read_only == true and
		(.services.queueframe.cap_drop | index("ALL") != null) and
		.services.queueframe.stop_grace_period == "1m30s" and
		.services.queueframe.environment.QUEUEFRAME_TRUSTED_PROXY_CIDRS == "172.30.255.0/24,127.0.0.0/8,::1/128" and
		.networks.queueframe.ipam.config[0].subnet == "172.30.255.0/24" and
		(.volumes | has("queueframe-data")) and
		(.volumes | has("caddy-data")) and
		(.volumes | has("caddy-config"))
	' "$TEST_DIR/compose.json" >/dev/null || fail_test 'Compose security or persistence invariants changed'
	jq -e '
		(.services.queueframe.ports // [] | length) == 0 and
		.services.tailscale.network_mode == "service:queueframe" and
		.services.tailscale.read_only == true and
		(.services.tailscale.cap_drop | index("ALL") != null) and
		(.volumes | has("queueframe-data")) and
		(.volumes | has("tailscale-state"))
	' "$TEST_DIR/compose-tailscale.json" >/dev/null || fail_test 'Tailscale isolation or persistence invariants changed'
	jq -e '
		.TCP["443"].HTTPS == true and
		.Web["${TS_CERT_DOMAIN}:443"].Handlers["/"].Proxy == "http://127.0.0.1:8080" and
		.AllowFunnel["${TS_CERT_DOMAIN}:443"] == false
	' "$SCRIPT_DIR/tailscale/serve.json" >/dev/null || fail_test 'Tailscale Serve is not private HTTPS to loopback'
	ok 'Compose and Tailscale security/persistence invariants hold'
else
	grep -q 'profile shortlived' "$SCRIPT_DIR/caddy/Caddyfile.public-ip" || fail_test 'short-lived certificate profile is absent'
	ok 'static deployment invariants hold (install jq for structural checks)'
fi

grep -Fq 'profile shortlived' "$SCRIPT_DIR/caddy/Caddyfile.public-ip" || fail_test 'public-IP Caddyfile is missing the shortlived ACME profile'
grep -Fq 'sha256:5f5c8640aae01df9654968d946d8f1a56c497f1dd5c5cda4cf95ab7c14d58648' "$SCRIPT_DIR/compose.yaml" || fail_test 'Caddy image is not digest-pinned'
grep -Fq 'sha256:f15d5d3f4a68773a853180b72496f70ba614b64de0878c43fe3da39fe0afba47' "$SCRIPT_DIR/compose.yaml" || fail_test 'Tailscale image is not digest-pinned'
ok 'proxy images are digest-pinned and IP certificates select shortlived'

grep -Fq 'header_up X-Forwarded-For {remote_host}' "$SCRIPT_DIR/caddy/Caddyfile.domain" || fail_test 'domain Caddy does not replace client-supplied forwarding chains'
grep -Fq 'header_up X-Forwarded-For {remote_host}' "$SCRIPT_DIR/caddy/Caddyfile.public-ip" || fail_test 'public-IP Caddy does not replace client-supplied forwarding chains'
# These assertions intentionally match unexpanded Compose variable syntax.
# shellcheck disable=SC2016
grep -Fq 'QUEUEFRAME_TRUSTED_PROXY_CIDRS: ${QUEUEFRAME_DOCKER_SUBNET:-172.30.255.0/24},127.0.0.0/8,::1/128' "$SCRIPT_DIR/compose.yaml" || fail_test 'Queueframe does not limit trusted proxies to its dedicated bridge plus loopback'
# shellcheck disable=SC2016
grep -Fq 'subnet: ${QUEUEFRAME_DOCKER_SUBNET:-172.30.255.0/24}' "$SCRIPT_DIR/compose.yaml" || fail_test 'Compose IPAM does not use the dedicated proxy subnet'
ok 'proxy trust is limited to the dedicated bridge and sanitized forwarding headers'

grep -Fq -- '--force-recreate' "$SCRIPT_DIR/seal.sh" || fail_test 'seal does not recreate containers after scrubbing dotenv'
grep -Fq 'docker inspect' "$SCRIPT_DIR/seal.sh" || fail_test 'seal does not inspect fresh container environments'
grep -Fq 'compose ps --all -q' "$SCRIPT_DIR/seal.sh" || fail_test 'seal does not inspect stopped container metadata after a prior/manual scrub'
grep -Fq 'existing_container_env_is_empty queueframe QUEUEFRAME_ADMIN_PASSWORD' "$SCRIPT_DIR/seal.sh" || fail_test 'seal does not detect a stale Queueframe bootstrap value on idempotent runs'
grep -Fq 'STALE_CONTAINER_CREDENTIALS=true' "$SCRIPT_DIR/seal.sh" || fail_test 'seal does not recreate already-sealed stacks with stale container metadata'
grep -Fq 'container_env_is_empty queueframe QUEUEFRAME_ADMIN_PASSWORD' "$SCRIPT_DIR/seal.sh" || fail_test 'seal does not verify the Queueframe bootstrap value is empty'
grep -Fq 'container_env_is_empty tailscale TS_AUTHKEY' "$SCRIPT_DIR/seal.sh" || fail_test 'seal does not verify the Tailscale auth key is empty'
# This assertion intentionally matches shell variable syntax literally.
# shellcheck disable=SC2016
grep -Fq 'TAILSCALE_ACTUAL_DNS" = "$TAILSCALE_FQDN_VALUE' "$SCRIPT_DIR/doctor.sh" || fail_test 'running doctor does not compare actual and configured Tailscale DNS names'
ok 'seal repairs prior/manual scrubs and inspects fresh containers; doctor binds Tailscale to its actual DNS name'

sh -n "$SCRIPT_DIR/lib.sh" "$SCRIPT_DIR/setup.sh" "$SCRIPT_DIR/doctor.sh" "$SCRIPT_DIR/seal.sh" "$SCRIPT_DIR/test.sh"
if command -v shfmt >/dev/null 2>&1; then
	shfmt -d "$SCRIPT_DIR/lib.sh" "$SCRIPT_DIR/setup.sh" "$SCRIPT_DIR/doctor.sh" "$SCRIPT_DIR/seal.sh" "$SCRIPT_DIR/test.sh" >/dev/null || fail_test 'deployment scripts are not shfmt-clean'
fi
if command -v shellcheck >/dev/null 2>&1; then
	shellcheck -x "$SCRIPT_DIR/lib.sh" "$SCRIPT_DIR/setup.sh" "$SCRIPT_DIR/doctor.sh" "$SCRIPT_DIR/seal.sh" "$SCRIPT_DIR/test.sh" || fail_test 'ShellCheck failed'
fi
ok 'deployment shell scripts pass available syntax and lint checks'

if docker info >/dev/null 2>&1; then
	docker run --rm \
		--env ACME_EMAIL=admin@example.com \
		--env QUEUEFRAME_HOSTNAME=queueframe.example.com \
		--volume "$SCRIPT_DIR/caddy/Caddyfile.domain:/etc/caddy/Caddyfile:ro" \
		"$CADDY_IMAGE" caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 || fail_test 'domain Caddyfile validation failed'
	docker run --rm \
		--env ACME_EMAIL=admin@example.com \
		--env QUEUEFRAME_CADDY_IP_SITE=https://203.0.113.10 \
		--volume "$SCRIPT_DIR/caddy/Caddyfile.public-ip:/etc/caddy/Caddyfile:ro" \
		"$CADDY_IMAGE" caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile >/dev/null 2>&1 || fail_test 'public-IP Caddyfile validation failed'
	ok 'both Caddyfiles validate with the pinned Caddy image'
else
	printf 'skip - Caddy runtime validation (Docker daemon unavailable)\n'
fi

printf '\n%d deployment checks passed.\n' "$PASS_COUNT"
