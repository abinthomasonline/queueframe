#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ENV_FILE=${QUEUEFRAME_DEPLOY_ENV_FILE:-"$SCRIPT_DIR/.env"}

# shellcheck source=deploy/lib.sh
. "$SCRIPT_DIR/lib.sh"

TEMP_FILE=
ECHO_DISABLED=false

cleanup() {
	if [ "$ECHO_DISABLED" = true ]; then
		stty echo 2>/dev/null || true
	fi
	if [ -n "$TEMP_FILE" ] && [ -f "$TEMP_FILE" ]; then
		rm -f -- "$TEMP_FILE"
	fi
}
trap cleanup EXIT HUP INT TERM

prompt() {
	_prompt_text=$1
	_prompt_default=${2-}
	if [ -n "$_prompt_default" ]; then
		printf '%s [%s]: ' "$_prompt_text" "$_prompt_default"
	else
		printf '%s: ' "$_prompt_text"
	fi
	IFS= read -r REPLY || return 1
	if [ -z "$REPLY" ] && [ -n "$_prompt_default" ]; then
		REPLY=$_prompt_default
	fi
}

prompt_secret() {
	_prompt_text=$1
	printf '%s: ' "$_prompt_text"
	if [ -t 0 ] && [ -t 1 ]; then
		stty -echo
		ECHO_DISABLED=true
		IFS= read -r REPLY || {
			stty echo
			ECHO_DISABLED=false
			return 1
		}
		stty echo
		ECHO_DISABLED=false
		printf '\n'
	else
		IFS= read -r REPLY || return 1
	fi
}

if [ -e "$ENV_FILE" ]; then
	printf 'Deployment configuration already exists at %s; nothing was overwritten.\n' "$ENV_FILE"
	printf 'Run %s/doctor.sh to validate it, or move it aside before a fresh setup.\n' "$SCRIPT_DIR"
	exit 0
fi

if ! command -v openssl >/dev/null 2>&1; then
	printf 'openssl is required to generate the Queueframe master key.\n' >&2
	exit 1
fi

printf '%s\n' 'Queueframe self-host setup'
printf '%s\n' '  1) Domain + Caddy (recommended for family access)'
printf '%s\n' '  2) Tailscale Serve (private, no domain)'
printf '%s\n' '  3) Public static IP + Caddy (advanced, short-lived IP certificate)'
prompt 'Access mode' '1'
case $REPLY in
1 | domain) DEPLOY_MODE=domain ;;
2 | tailscale) DEPLOY_MODE=tailscale ;;
3 | public-ip | ip) DEPLOY_MODE=public-ip ;;
*)
	printf 'Unknown access mode. Choose 1, 2, or 3.\n' >&2
	exit 1
	;;
esac

prompt 'Administrator username' 'admin'
ADMIN_USER=$REPLY
qf_valid_node_name "$ADMIN_USER" || {
	printf 'Administrator username must use 1-63 letters, numbers, or hyphens.\n' >&2
	exit 1
}

prompt_secret 'Administrator password (minimum 12 characters)'
ADMIN_PASSWORD=$REPLY
[ "${#ADMIN_PASSWORD}" -ge 12 ] || {
	printf 'Administrator password must contain at least 12 characters.\n' >&2
	exit 1
}
prompt_secret 'Confirm administrator password'
[ "$REPLY" = "$ADMIN_PASSWORD" ] || {
	printf 'Administrator passwords did not match.\n' >&2
	exit 1
}

prompt 'Static outbound IP whitelisted with Zerodha'
EXPECTED_IP=$REPLY
qf_valid_ip "$EXPECTED_IP" || {
	printf 'Expected outbound address must be one valid IPv4 or IPv6 address.\n' >&2
	exit 1
}

HOSTNAME_VALUE=
INGRESS_IP=
ACME_EMAIL_VALUE=
TAILSCALE_FQDN_VALUE=
TAILSCALE_HOSTNAME_VALUE=
TAILSCALE_AUTHKEY_VALUE=
CADDY_IP_SITE=

case $DEPLOY_MODE in
domain)
	prompt 'Public hostname (for example queueframe.example.com)'
	HOSTNAME_VALUE=$(qf_lower "$REPLY")
	qf_valid_domain "$HOSTNAME_VALUE" || {
		printf 'Public hostname must be a valid DNS name, without a scheme or path.\n' >&2
		exit 1
	}
	prompt 'Public ingress IP used by that DNS record' "$EXPECTED_IP"
	INGRESS_IP=$REPLY
	qf_valid_ip "$INGRESS_IP" || {
		printf 'Ingress address must be one valid IPv4 or IPv6 address.\n' >&2
		exit 1
	}
	prompt "Let's Encrypt contact email"
	ACME_EMAIL_VALUE=$REPLY
	qf_valid_email "$ACME_EMAIL_VALUE" || {
		printf 'Enter a valid contact email address.\n' >&2
		exit 1
	}
	BASE_URL=https://$HOSTNAME_VALUE
	;;
tailscale)
	prompt 'Full Tailscale HTTPS name (for example queueframe.example-tailnet.ts.net)'
	TAILSCALE_FQDN_VALUE=$(qf_lower "$REPLY")
	qf_valid_domain "$TAILSCALE_FQDN_VALUE" || {
		printf 'Tailscale name must be a valid DNS name.\n' >&2
		exit 1
	}
	case $TAILSCALE_FQDN_VALUE in
	*.ts.net) ;;
	*)
		printf 'Tailscale HTTPS names must end in .ts.net.\n' >&2
		exit 1
		;;
	esac
	prompt 'Tailscale node hostname' "${TAILSCALE_FQDN_VALUE%%.*}"
	TAILSCALE_HOSTNAME_VALUE=$(qf_lower "$REPLY")
	qf_valid_node_name "$TAILSCALE_HOSTNAME_VALUE" || {
		printf 'Tailscale hostname must use 1-63 letters, numbers, or hyphens.\n' >&2
		exit 1
	}
	prompt_secret 'One-off, pre-authorized Tailscale auth key'
	TAILSCALE_AUTHKEY_VALUE=$REPLY
	case $TAILSCALE_AUTHKEY_VALUE in
	tskey-*) ;;
	*)
		printf 'Tailscale auth key must begin with tskey-.\n' >&2
		exit 1
		;;
	esac
	BASE_URL=https://$TAILSCALE_FQDN_VALUE
	;;
public-ip)
	prompt 'Public ingress IP used in the browser URL' "$EXPECTED_IP"
	INGRESS_IP=$REPLY
	qf_valid_ip "$INGRESS_IP" || {
		printf 'Ingress address must be one valid IPv4 or IPv6 address.\n' >&2
		exit 1
	}
	prompt "Let's Encrypt contact email"
	ACME_EMAIL_VALUE=$REPLY
	qf_valid_email "$ACME_EMAIL_VALUE" || {
		printf 'Enter a valid contact email address.\n' >&2
		exit 1
	}
	CADDY_IP_SITE=$(qf_url_for_ip "$INGRESS_IP")
	BASE_URL=$CADDY_IP_SITE
	;;
esac

qf_valid_base_url "$BASE_URL" || {
	printf 'Derived browser URL is invalid; no configuration was written.\n' >&2
	exit 1
}

MASTER_KEY=$(openssl rand -base64 32 | tr -d '\n')
[ -n "$MASTER_KEY" ] || {
	printf 'Unable to generate the Queueframe master key.\n' >&2
	exit 1
}

umask 077
TEMP_FILE=$(mktemp "${ENV_FILE}.tmp.XXXXXX")
chmod 600 "$TEMP_FILE"
{
	printf '%s\n' '# Generated by deploy/setup.sh. Do not commit or share this file.'
	printf '%s\n' '# Keep QUEUEFRAME_DRY_RUN=true through at least one full rehearsal.'
} >>"$TEMP_FILE"
qf_dotenv_write "$TEMP_FILE" COMPOSE_PROJECT_NAME queueframe
qf_dotenv_write "$TEMP_FILE" COMPOSE_PROFILES "$DEPLOY_MODE"
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_DEPLOY_MODE "$DEPLOY_MODE"
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_IMAGE queueframe:local
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_BASE_URL "$BASE_URL"
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_MASTER_KEY "$MASTER_KEY"
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_ADMIN_USER "$ADMIN_USER"
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_ADMIN_PASSWORD "$ADMIN_PASSWORD"
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_SECURE_COOKIES true
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_EXPECTED_PUBLIC_IP "$EXPECTED_IP"
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_DOCKER_SUBNET 172.30.255.0/24
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_DRY_RUN true
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_WARM_BEFORE 10s
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_LATE_CUTOFF 5s
qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_WEB_SESSION_TTL 12h

case $DEPLOY_MODE in
domain)
	qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_HOSTNAME "$HOSTNAME_VALUE"
	qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_INGRESS_IP "$INGRESS_IP"
	qf_dotenv_write "$TEMP_FILE" ACME_EMAIL "$ACME_EMAIL_VALUE"
	;;
tailscale)
	qf_dotenv_write "$TEMP_FILE" TAILSCALE_FQDN "$TAILSCALE_FQDN_VALUE"
	qf_dotenv_write "$TEMP_FILE" TAILSCALE_HOSTNAME "$TAILSCALE_HOSTNAME_VALUE"
	qf_dotenv_write "$TEMP_FILE" TAILSCALE_AUTHKEY "$TAILSCALE_AUTHKEY_VALUE"
	;;
public-ip)
	qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_INGRESS_IP "$INGRESS_IP"
	qf_dotenv_write "$TEMP_FILE" QUEUEFRAME_CADDY_IP_SITE "$CADDY_IP_SITE"
	qf_dotenv_write "$TEMP_FILE" ACME_EMAIL "$ACME_EMAIL_VALUE"
	;;
esac

mv -- "$TEMP_FILE" "$ENV_FILE"
TEMP_FILE=
chmod 600 "$ENV_FILE"

printf '\nConfiguration written to %s with mode 600.\n' "$ENV_FILE"
printf 'Browser URL: %s\n' "$BASE_URL"
printf 'Kite redirect URL: %s/auth/kite/callback\n' "$BASE_URL"
printf 'Dry-run remains enabled. Next run: %s/doctor.sh\n' "$SCRIPT_DIR"
