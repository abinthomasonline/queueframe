#!/bin/sh

set -u

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
ENV_FILE=${QUEUEFRAME_DEPLOY_ENV_FILE:-"$SCRIPT_DIR/.env"}
COMPOSE_FILE=$SCRIPT_DIR/compose.yaml
CHECK_RUNNING=false

# shellcheck source=deploy/lib.sh
. "$SCRIPT_DIR/lib.sh"

case ${1-} in
'') ;;
--running) CHECK_RUNNING=true ;;
-h | --help)
	printf 'Usage: %s [--running]\n' "$0"
	printf '  no argument  validate before the first start\n'
	printf '  --running    also validate the running containers and HTTPS endpoint\n'
	exit 0
	;;
*)
	printf 'Unknown option: %s\n' "$1" >&2
	exit 2
	;;
esac

PASSES=0
WARNINGS=0
FAILURES=0

pass() {
	PASSES=$((PASSES + 1))
	printf 'ok   %s\n' "$1"
}

warn() {
	WARNINGS=$((WARNINGS + 1))
	printf 'warn %s\n' "$1"
}

fail() {
	FAILURES=$((FAILURES + 1))
	printf 'FAIL %s\n' "$1"
}

env_value() {
	qf_dotenv_get "$ENV_FILE" "$1" 2>/dev/null || true
}

ip_equal() {
	_qf_left=$(qf_lower "$1")
	_qf_right=$(qf_lower "$2")
	[ "$_qf_left" = "$_qf_right" ] && return 0
	if command -v python3 >/dev/null 2>&1; then
		python3 - "$_qf_left" "$_qf_right" <<'PY' >/dev/null 2>&1
import ipaddress
import sys
raise SystemExit(0 if ipaddress.ip_address(sys.argv[1]) == ipaddress.ip_address(sys.argv[2]) else 1)
PY
		return $?
	fi
	return 1
}

resolve_host_ips() {
	_qf_resolve_name=$1
	if command -v getent >/dev/null 2>&1; then
		{
			getent ahostsv4 "$_qf_resolve_name" 2>/dev/null || true
			getent ahostsv6 "$_qf_resolve_name" 2>/dev/null || true
		} | awk '{print $1}' | sort -u
		return
	fi
	if command -v dig >/dev/null 2>&1; then
		{
			dig +short A "$_qf_resolve_name" 2>/dev/null || true
			dig +short AAAA "$_qf_resolve_name" 2>/dev/null || true
		} | sort -u
		return
	fi
	if command -v dscacheutil >/dev/null 2>&1; then
		dscacheutil -q host -a name "$_qf_resolve_name" 2>/dev/null | awk '/ip_address:/ {print $2}' | sort -u
		return
	fi
	if command -v host >/dev/null 2>&1; then
		host "$_qf_resolve_name" 2>/dev/null | awk '/has (IPv6 )?address/ {print $NF}' | sort -u
		return
	fi
	return 2
}

tcp_port_in_use() {
	_qf_port=$1
	if command -v ss >/dev/null 2>&1; then
		ss -H -ltn 2>/dev/null | awk -v port="$_qf_port" '$4 ~ (":" port "$") {found = 1} END {exit !found}'
		return $?
	fi
	if command -v lsof >/dev/null 2>&1; then
		[ -n "$(lsof -nP -iTCP:"$_qf_port" -sTCP:LISTEN -t 2>/dev/null)" ]
		return $?
	fi
	if command -v netstat >/dev/null 2>&1; then
		netstat -an 2>/dev/null | awk -v port="$_qf_port" '$0 ~ /LISTEN/ && $4 ~ ("[.:]" port "$") {found = 1} END {exit !found}'
		return $?
	fi
	return 2
}

check_public_ports() {
	for _qf_port in 80 443; do
		tcp_port_in_use "$_qf_port"
		_qf_status=$?
		case $_qf_status in
		0) fail "TCP port $_qf_port is already occupied" ;;
		1) pass "TCP port $_qf_port is available" ;;
		*) warn "could not inspect TCP port $_qf_port; install ss or lsof" ;;
		esac
	done
}

check_clock() {
	if command -v timedatectl >/dev/null 2>&1; then
		_qf_ntp=$(timedatectl show -p NTPSynchronized --value 2>/dev/null || true)
		case $_qf_ntp in
		yes) pass 'host clock reports NTP synchronization' ;;
		no) fail 'host clock is not synchronized; fix NTP before scheduling orders' ;;
		*) warn 'timedatectl could not confirm clock synchronization' ;;
		esac
		return
	fi
	if command -v chronyc >/dev/null 2>&1; then
		if chronyc tracking 2>/dev/null | grep -q 'Leap status[[:space:]]*:[[:space:]]*Normal'; then
			pass 'chrony reports a synchronized host clock'
		else
			fail 'chrony does not report a synchronized host clock'
		fi
		return
	fi
	if command -v sntp >/dev/null 2>&1; then
		_qf_sntp=$(sntp -t 3 time.cloudflare.com 2>/dev/null || true)
		if [ -z "$_qf_sntp" ]; then
			warn 'could not compare the host clock with a network time source'
		elif printf '%s\n' "$_qf_sntp" | awk '
			NR == 1 {
				offset = $1 + 0
				if (offset < 0) offset = -offset
				exit(offset <= 1 ? 0 : 1)
			}
		'; then
			pass 'host clock is within one second of a network time source'
		else
			fail 'host clock differs from a network time source by more than one second'
		fi
		return
	fi
	warn 'could not verify clock synchronization; confirm the host NTP service manually'
}

check_dns() {
	_qf_dns_name=$1
	_qf_dns_expected=$2
	_qf_dns_ips=$(resolve_host_ips "$_qf_dns_name")
	_qf_dns_status=$?
	if [ "$_qf_dns_status" -eq 2 ]; then
		warn "could not inspect DNS for $_qf_dns_name; install getent or dig"
		return
	fi
	_qf_dns_match=false
	_qf_old_ifs=$IFS
	IFS='
'
	for _qf_dns_ip in $_qf_dns_ips; do
		qf_valid_ip "$_qf_dns_ip" || continue
		if ip_equal "$_qf_dns_ip" "$_qf_dns_expected"; then
			_qf_dns_match=true
			break
		fi
	done
	IFS=$_qf_old_ifs
	if [ "$_qf_dns_match" = true ]; then
		pass "DNS for $_qf_dns_name includes the configured ingress IP"
	elif [ -z "$_qf_dns_ips" ]; then
		fail "DNS for $_qf_dns_name does not resolve yet"
	else
		fail "DNS for $_qf_dns_name does not include the configured ingress IP"
	fi
}

check_egress() {
	_qf_expected=$1
	if ! command -v curl >/dev/null 2>&1; then
		warn 'curl is unavailable; public egress IP was not checked'
		return
	fi
	if qf_valid_ipv4 "$_qf_expected"; then
		_qf_curl_family=-4
	else
		_qf_curl_family=-6
	fi
	_qf_observed=$(curl "$_qf_curl_family" -fsS --max-time 6 https://www.cloudflare.com/cdn-cgi/trace 2>/dev/null | awk -F= '$1 == "ip" {print $2; exit}')
	if ! qf_valid_ip "$_qf_observed"; then
		warn 'could not observe this host public egress IP'
	elif ip_equal "$_qf_observed" "$_qf_expected"; then
		pass 'observed public egress IP matches Queueframe configuration'
	else
		fail 'observed public egress IP does not match Queueframe configuration'
	fi
}

printf 'Queueframe deployment doctor (%s)\n\n' "$(if [ "$CHECK_RUNNING" = true ]; then printf running; else printf preflight; fi)"

if [ ! -f "$ENV_FILE" ]; then
	fail "configuration is missing at $ENV_FILE; run $SCRIPT_DIR/setup.sh"
	printf '\n%d passed, %d warning(s), %d failure(s)\n' "$PASSES" "$WARNINGS" "$FAILURES"
	exit 1
fi

ENV_MODE=$(qf_file_mode "$ENV_FILE" 2>/dev/null || true)
case $ENV_MODE in
600 | 400) pass 'deploy/.env permissions are owner-only' ;;
*) fail "deploy/.env permissions are $ENV_MODE, expected 600 (or stricter 400)" ;;
esac

DEPLOY_MODE=$(env_value QUEUEFRAME_DEPLOY_MODE)
COMPOSE_PROFILES_VALUE=$(env_value COMPOSE_PROFILES)
BASE_URL=$(env_value QUEUEFRAME_BASE_URL)
MASTER_KEY=$(env_value QUEUEFRAME_MASTER_KEY)
ADMIN_USER=$(env_value QUEUEFRAME_ADMIN_USER)
ADMIN_PASSWORD=$(env_value QUEUEFRAME_ADMIN_PASSWORD)
SECURE_COOKIES=$(env_value QUEUEFRAME_SECURE_COOKIES)
EXPECTED_IP=$(env_value QUEUEFRAME_EXPECTED_PUBLIC_IP)
DOCKER_SUBNET=$(env_value QUEUEFRAME_DOCKER_SUBNET)
DRY_RUN=$(env_value QUEUEFRAME_DRY_RUN)

case $DEPLOY_MODE in
domain | tailscale | public-ip) pass "deployment mode is $DEPLOY_MODE" ;;
*) fail 'QUEUEFRAME_DEPLOY_MODE must be domain, tailscale, or public-ip' ;;
esac

if [ "$COMPOSE_PROFILES_VALUE" = "$DEPLOY_MODE" ]; then
	pass 'Compose profile matches deployment mode'
else
	fail 'COMPOSE_PROFILES must exactly match QUEUEFRAME_DEPLOY_MODE'
fi

if qf_valid_base_url "$BASE_URL"; then
	pass 'browser-facing base URL is an origin-only HTTPS URL'
else
	fail 'QUEUEFRAME_BASE_URL must be one origin-only HTTPS URL'
fi

if [ "$SECURE_COOKIES" = true ]; then
	pass 'secure cookies are required by the HTTPS deployment'
else
	fail 'QUEUEFRAME_SECURE_COOKIES must be true'
fi

if [ "$DRY_RUN" = true ]; then
	pass 'dry-run safety is enabled'
else
	warn 'live order placement is enabled; restore dry-run unless rehearsal is complete'
fi

if qf_valid_ip "$EXPECTED_IP"; then
	pass 'expected public egress address is syntactically valid'
else
	fail 'QUEUEFRAME_EXPECTED_PUBLIC_IP is not one valid IP address'
fi

if qf_valid_private_docker_subnet "$DOCKER_SUBNET"; then
	pass 'dedicated trusted-proxy Docker subnet is a narrow private IPv4 network'
else
	fail 'QUEUEFRAME_DOCKER_SUBNET must be an aligned private IPv4 /24 through /29'
fi

MASTER_BYTES=$(printf '%s' "$MASTER_KEY" | openssl base64 -d -A 2>/dev/null | wc -c | tr -d '[:space:]')
if [ "$MASTER_BYTES" = 32 ]; then
	pass 'master key decodes to exactly 32 bytes'
else
	fail 'QUEUEFRAME_MASTER_KEY is not a standard-base64 32-byte key'
fi

if qf_valid_node_name "$ADMIN_USER"; then
	pass 'administrator username is valid'
else
	fail 'administrator username is empty or contains unsupported characters'
fi

if [ -z "$ADMIN_PASSWORD" ]; then
	if [ "$CHECK_RUNNING" = true ]; then
		pass 'bootstrap administrator password is not stored in deployment configuration'
	else
		warn 'bootstrap administrator password is empty; this is valid only after the database has an administrator'
	fi
elif [ "${#ADMIN_PASSWORD}" -ge 12 ]; then
	warn 'bootstrap administrator password remains stored; run deploy/seal.sh after healthy first boot'
else
	fail 'bootstrap administrator password must contain at least 12 characters'
fi

case $DEPLOY_MODE in
domain)
	HOSTNAME_VALUE=$(env_value QUEUEFRAME_HOSTNAME)
	INGRESS_IP=$(env_value QUEUEFRAME_INGRESS_IP)
	ACME_EMAIL_VALUE=$(env_value ACME_EMAIL)
	if qf_valid_domain "$HOSTNAME_VALUE"; then
		pass 'domain hostname is valid'
	else
		fail 'QUEUEFRAME_HOSTNAME is invalid'
	fi
	if qf_valid_ip "$INGRESS_IP"; then
		pass 'domain ingress IP is valid'
	else
		fail 'QUEUEFRAME_INGRESS_IP is invalid'
	fi
	if qf_valid_email "$ACME_EMAIL_VALUE"; then
		pass 'ACME contact email is valid'
	else
		fail 'ACME_EMAIL is invalid'
	fi
	if [ "$BASE_URL" = "https://$HOSTNAME_VALUE" ]; then
		pass 'base URL matches domain hostname'
	else
		fail 'base URL does not match domain hostname'
	fi
	if qf_valid_domain "$HOSTNAME_VALUE" && qf_valid_ip "$INGRESS_IP"; then
		check_dns "$HOSTNAME_VALUE" "$INGRESS_IP"
	fi
	if [ "$CHECK_RUNNING" = false ]; then
		check_public_ports
	fi
	;;
tailscale)
	TAILSCALE_FQDN_VALUE=$(env_value TAILSCALE_FQDN)
	TAILSCALE_HOSTNAME_VALUE=$(env_value TAILSCALE_HOSTNAME)
	TAILSCALE_AUTHKEY_VALUE=$(env_value TAILSCALE_AUTHKEY)
	case $TAILSCALE_FQDN_VALUE in
	*.ts.net)
		if qf_valid_domain "$TAILSCALE_FQDN_VALUE"; then
			pass 'Tailscale HTTPS name is valid'
		else
			fail 'TAILSCALE_FQDN is invalid'
		fi
		;;
	*) fail 'TAILSCALE_FQDN must be a full .ts.net name' ;;
	esac
	if qf_valid_node_name "$TAILSCALE_HOSTNAME_VALUE"; then
		pass 'Tailscale node hostname is valid'
	else
		fail 'TAILSCALE_HOSTNAME is invalid'
	fi
	if [ "$BASE_URL" = "https://$TAILSCALE_FQDN_VALUE" ]; then
		pass 'base URL matches Tailscale HTTPS name'
	else
		fail 'base URL does not match Tailscale HTTPS name'
	fi
	if [ -n "$TAILSCALE_AUTHKEY_VALUE" ]; then
		case $TAILSCALE_AUTHKEY_VALUE in
		tskey-*) warn 'Tailscale auth key remains stored; run deploy/seal.sh after healthy initial join' ;;
		*) fail 'TAILSCALE_AUTHKEY has an unexpected format' ;;
		esac
	elif [ "$CHECK_RUNNING" = false ]; then
		warn 'Tailscale auth key is empty; this requires an already-persisted node identity'
	else
		pass 'Tailscale auth key is not stored in deployment configuration'
	fi
	;;
public-ip)
	INGRESS_IP=$(env_value QUEUEFRAME_INGRESS_IP)
	CADDY_IP_SITE=$(env_value QUEUEFRAME_CADDY_IP_SITE)
	ACME_EMAIL_VALUE=$(env_value ACME_EMAIL)
	if qf_valid_ip "$INGRESS_IP"; then
		pass 'public ingress IP is valid'
	else
		fail 'QUEUEFRAME_INGRESS_IP is invalid'
	fi
	if qf_valid_email "$ACME_EMAIL_VALUE"; then
		pass 'ACME contact email is valid'
	else
		fail 'ACME_EMAIL is invalid'
	fi
	EXPECTED_IP_SITE=$(qf_url_for_ip "$INGRESS_IP")
	if [ "$BASE_URL" = "$EXPECTED_IP_SITE" ]; then
		pass 'base URL matches public ingress IP'
	else
		fail 'base URL does not match public ingress IP'
	fi
	if [ "$CADDY_IP_SITE" = "$EXPECTED_IP_SITE" ]; then
		pass 'Caddy site address matches public ingress IP'
	else
		fail 'QUEUEFRAME_CADDY_IP_SITE does not match public ingress IP'
	fi
	if [ "$CHECK_RUNNING" = false ]; then
		check_public_ports
	fi
	;;
esac

check_clock
if qf_valid_ip "$EXPECTED_IP"; then
	check_egress "$EXPECTED_IP"
fi

if ! command -v docker >/dev/null 2>&1; then
	fail 'Docker is not installed'
elif ! docker info >/dev/null 2>&1; then
	fail 'Docker daemon is not reachable by this user'
elif ! docker compose version >/dev/null 2>&1; then
	fail 'Docker Compose v2 is not available'
else
	pass 'Docker Engine and Compose are available'
	if docker compose --project-directory "$SCRIPT_DIR" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" config --quiet >/dev/null 2>&1; then
		pass 'Compose configuration renders successfully'
	else
		fail 'Compose configuration did not render; run Docker Compose config for details'
	fi
fi

if [ "$CHECK_RUNNING" = true ] && command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
	RUNNING_SERVICES=$(docker compose --project-directory "$SCRIPT_DIR" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" ps --status running --services 2>/dev/null || true)
	if printf '%s\n' "$RUNNING_SERVICES" | grep -qx queueframe; then
		pass 'Queueframe container is running'
	else
		fail 'Queueframe container is not running'
	fi
	case $DEPLOY_MODE in
	domain)
		if printf '%s\n' "$RUNNING_SERVICES" | grep -qx caddy-domain; then
			pass 'domain Caddy container is running'
		else
			fail 'domain Caddy container is not running'
		fi
		;;
	public-ip)
		if printf '%s\n' "$RUNNING_SERVICES" | grep -qx caddy-public-ip; then
			pass 'public-IP Caddy container is running'
		else
			fail 'public-IP Caddy container is not running'
		fi
		;;
	tailscale)
		if printf '%s\n' "$RUNNING_SERVICES" | grep -qx tailscale; then
			pass 'Tailscale container is running'
		else
			fail 'Tailscale container is not running'
		fi
		TAILSCALE_STATUS_JSON=$(docker compose --project-directory "$SCRIPT_DIR" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" exec -T tailscale tailscale status --json 2>/dev/null || true)
		TAILSCALE_BACKEND=$(printf '%s\n' "$TAILSCALE_STATUS_JSON" | qf_tailscale_backend_from_status)
		TAILSCALE_ACTUAL_DNS=$(printf '%s\n' "$TAILSCALE_STATUS_JSON" | qf_tailscale_dns_from_status)
		if [ "$TAILSCALE_BACKEND" = Running ]; then
			pass 'Tailscale backend state is Running'
		else
			fail 'Tailscale backend state is not Running'
		fi
		if [ "$TAILSCALE_ACTUAL_DNS" = "$TAILSCALE_FQDN_VALUE" ]; then
			pass 'joined Tailscale node DNS name exactly matches the Queueframe base URL'
		else
			fail 'joined Tailscale node DNS name does not match TAILSCALE_FQDN (check for a typo or hostname collision)'
		fi
		if docker compose --project-directory "$SCRIPT_DIR" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" exec -T tailscale wget -q -O /dev/null http://127.0.0.1:9002/healthz >/dev/null 2>&1; then
			pass 'Tailscale container health endpoint confirms a tailnet address'
		else
			fail 'Tailscale container health endpoint is not ready'
		fi
		if docker compose --project-directory "$SCRIPT_DIR" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" exec -T tailscale tailscale serve status 2>/dev/null | grep -q '127.0.0.1:8080'; then
			pass 'Tailscale Serve proxies Queueframe privately'
		else
			fail 'Tailscale Serve is not proxying Queueframe on HTTPS'
		fi
		;;
	esac
	if [ "$DEPLOY_MODE" = tailscale ]; then
		if docker compose --project-directory "$SCRIPT_DIR" --env-file "$ENV_FILE" -f "$COMPOSE_FILE" exec -T tailscale wget -q -O /dev/null http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
			pass 'Queueframe health endpoint answers inside the private network namespace'
		else
			fail 'Queueframe health endpoint does not answer the Tailscale sidecar'
		fi
	elif command -v curl >/dev/null 2>&1; then
		if curl -fsS --max-time 15 "$BASE_URL/healthz" >/dev/null 2>&1; then
			pass 'public HTTPS health endpoint has a trusted certificate and answers'
		else
			fail 'public HTTPS health endpoint is unavailable or its certificate is untrusted'
		fi
	else
		warn 'curl is unavailable; the public HTTPS health endpoint was not checked'
	fi
fi

printf '\n%d passed, %d warning(s), %d failure(s)\n' "$PASSES" "$WARNINGS" "$FAILURES"
[ "$FAILURES" -eq 0 ]
