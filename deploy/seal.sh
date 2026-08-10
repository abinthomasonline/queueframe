#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ENV_FILE=${QUEUEFRAME_DEPLOY_ENV_FILE:-"$SCRIPT_DIR/.env"}
COMPOSE_FILE=$SCRIPT_DIR/compose.yaml

# shellcheck source=deploy/lib.sh
. "$SCRIPT_DIR/lib.sh"

compose() {
	docker compose --project-directory "$SCRIPT_DIR" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" "$@"
}

fail() {
	printf 'Refusing to seal deployment credentials: %s\n' "$1" >&2
	exit 1
}

recovery_fail() {
	printf 'Bootstrap credential fields are absent from %s, but fresh containers did not become healthy and clean.\n' "$ENV_FILE" >&2
	printf 'The master key and persistent volumes remain intact. Inspect logs, then retry:\n' >&2
	printf '  docker compose --project-directory "%s" --env-file "%s" -f "%s" up --no-build --force-recreate -d\n' \
		"$SCRIPT_DIR" "$ENV_FILE" "$COMPOSE_FILE" >&2
	exit 1
}

tailscale_is_ready() {
	_qf_seal_status=$(compose exec -T tailscale tailscale status --json 2>/dev/null || true)
	_qf_seal_backend=$(printf '%s\n' "$_qf_seal_status" | qf_tailscale_backend_from_status)
	_qf_seal_dns=$(printf '%s\n' "$_qf_seal_status" | qf_tailscale_dns_from_status)
	[ "$_qf_seal_backend" = Running ] || return 1
	[ "$_qf_seal_dns" = "$TAILSCALE_FQDN_VALUE" ] || return 1
	compose exec -T tailscale wget -q -O /dev/null http://127.0.0.1:9002/healthz >/dev/null 2>&1 || return 1
	compose exec -T tailscale tailscale serve status 2>/dev/null | grep -q '127.0.0.1:8080'
}

stack_is_healthy() {
	_qf_seal_running=$(compose ps --status running --services 2>/dev/null || true)
	printf '%s\n' "$_qf_seal_running" | grep -qx queueframe || return 1
	printf '%s\n' "$_qf_seal_running" | grep -qx "$INGRESS_SERVICE" || return 1
	case $DEPLOY_MODE in
	domain | public-ip)
		compose exec -T "$INGRESS_SERVICE" wget -q -O /dev/null http://queueframe:8080/healthz >/dev/null 2>&1
		;;
	tailscale)
		compose exec -T tailscale wget -q -O /dev/null http://127.0.0.1:8080/healthz >/dev/null 2>&1 && tailscale_is_ready
		;;
	esac
}

service_has_existing_container() {
	[ -n "$(compose ps --all -q "$1" 2>/dev/null || true)" ]
}

service_containers_env_are_empty() {
	_qf_seal_scope=$1
	_qf_seal_service=$2
	_qf_seal_key=$3
	case $_qf_seal_scope in
	all) _qf_seal_containers=$(compose ps --all -q "$_qf_seal_service" 2>/dev/null || true) ;;
	running) _qf_seal_containers=$(compose ps --status running -q "$_qf_seal_service" 2>/dev/null || true) ;;
	*) return 1 ;;
	esac
	[ -n "$_qf_seal_containers" ] || return 1
	for _qf_seal_container in $_qf_seal_containers; do
		docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$_qf_seal_container" 2>/dev/null |
			awk -v key="$_qf_seal_key" '
			index($0, key "=") == 1 {
				found = 1
				if ($0 != key "=") bad = 1
			}
			END { exit(!found || bad) }
			' || return 1
	done
}

existing_container_env_is_empty() {
	service_containers_env_are_empty all "$1" "$2"
}

container_env_is_empty() {
	service_containers_env_are_empty running "$1" "$2"
}

[ -f "$ENV_FILE" ] || fail "configuration is missing at $ENV_FILE"

DEPLOY_MODE=$(qf_dotenv_get "$ENV_FILE" QUEUEFRAME_DEPLOY_MODE 2>/dev/null || true)
case $DEPLOY_MODE in
domain) INGRESS_SERVICE=caddy-domain ;;
public-ip) INGRESS_SERVICE=caddy-public-ip ;;
tailscale)
	INGRESS_SERVICE=tailscale
	TAILSCALE_FQDN_VALUE=$(qf_dotenv_get "$ENV_FILE" TAILSCALE_FQDN 2>/dev/null || true)
	qf_valid_domain "$TAILSCALE_FQDN_VALUE" || fail 'TAILSCALE_FQDN is invalid'
	;;
*) fail 'deployment mode is invalid' ;;
esac

command -v docker >/dev/null 2>&1 || fail 'Docker is not installed'
docker info >/dev/null 2>&1 || fail 'Docker daemon is not reachable by this user'
compose config --quiet >/dev/null 2>&1 || fail 'Compose configuration does not render'
[ -n "$(qf_dotenv_get "$ENV_FILE" QUEUEFRAME_MASTER_KEY 2>/dev/null || true)" ] ||
	fail 'configuration does not contain a readable master key'

CREDENTIAL_FIELDS_PRESENT=false
if grep -Eq '^(QUEUEFRAME_ADMIN_PASSWORD|TAILSCALE_AUTHKEY)=' "$ENV_FILE"; then
	CREDENTIAL_FIELDS_PRESENT=true
fi

if [ "$CREDENTIAL_FIELDS_PRESENT" = true ]; then
	RUNNING_SERVICES=$(compose ps --status running --services 2>/dev/null || true)
	printf '%s\n' "$RUNNING_SERVICES" | grep -qx queueframe || fail 'Queueframe is not running'
	printf '%s\n' "$RUNNING_SERVICES" | grep -qx "$INGRESS_SERVICE" || fail "$INGRESS_SERVICE is not running"

	case $DEPLOY_MODE in
	domain | public-ip)
		compose exec -T "$INGRESS_SERVICE" wget -q -O /dev/null http://queueframe:8080/healthz >/dev/null 2>&1 ||
			fail 'the selected ingress cannot reach Queueframe health'
		;;
	tailscale)
		compose exec -T tailscale wget -q -O /dev/null http://127.0.0.1:8080/healthz >/dev/null 2>&1 ||
			fail 'the Tailscale sidecar cannot reach Queueframe health'
		tailscale_is_ready || fail 'Tailscale must be Running with a healthy address, active Serve, and a DNS name exactly matching TAILSCALE_FQDN'
		;;
	esac

	umask 077
	qf_scrub_bootstrap_credentials "$ENV_FILE" || fail 'atomic credential removal failed; the original file was retained'

	if grep -Eq '^(QUEUEFRAME_ADMIN_PASSWORD|TAILSCALE_AUTHKEY)=' "$ENV_FILE"; then
		fail 'post-write verification found a bootstrap credential key'
	fi
	[ -n "$(qf_dotenv_get "$ENV_FILE" QUEUEFRAME_MASTER_KEY 2>/dev/null || true)" ] ||
		fail 'post-write verification could not read the master key'
else
	CONTAINER_METADATA_EXISTS=false
	STALE_CONTAINER_CREDENTIALS=false
	if service_has_existing_container queueframe; then
		CONTAINER_METADATA_EXISTS=true
		existing_container_env_is_empty queueframe QUEUEFRAME_ADMIN_PASSWORD || STALE_CONTAINER_CREDENTIALS=true
	fi
	if [ "$DEPLOY_MODE" = tailscale ] && service_has_existing_container tailscale; then
		CONTAINER_METADATA_EXISTS=true
		existing_container_env_is_empty tailscale TS_AUTHKEY || STALE_CONTAINER_CREDENTIALS=true
	fi

	if [ "$STALE_CONTAINER_CREDENTIALS" = false ]; then
		if [ "$CONTAINER_METADATA_EXISTS" = true ]; then
			printf 'Deployment configuration and existing container metadata are already sealed; nothing changed.\n'
		else
			printf 'Deployment configuration is already sealed and no selected container metadata exists; nothing changed.\n'
		fi
		exit 0
	fi
	printf 'Deployment configuration is sealed, but stale bootstrap credential metadata was found; recreating the selected stack.\n'
fi

if ! compose up --no-build --force-recreate -d >/dev/null 2>&1; then
	recovery_fail
fi

ATTEMPT=0
while ! stack_is_healthy; do
	ATTEMPT=$((ATTEMPT + 1))
	[ "$ATTEMPT" -lt 30 ] || recovery_fail
	sleep 2
done

container_env_is_empty queueframe QUEUEFRAME_ADMIN_PASSWORD || recovery_fail
if [ "$DEPLOY_MODE" = tailscale ]; then
	container_env_is_empty tailscale TS_AUTHKEY || recovery_fail
fi

printf 'Deployment configuration and container metadata are sealed.\n'
printf 'Bootstrap administrator and Tailscale auth-key fields are absent; the master key was retained.\n'
printf 'Fresh healthy containers now run with empty bootstrap credential values.\n'
