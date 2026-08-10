#!/bin/sh

# Shared, POSIX-shell helpers for setup, diagnostics, and tests. This file does
# not read deploy/.env into the shell: a populated dotenv file is data, not
# trusted shell code.

qf_lower() {
	printf '%s' "$1" | tr '[:upper:]' '[:lower:]'
}

qf_valid_hostname() {
	printf '%s\n' "$1" | awk '
		length($0) < 1 || length($0) > 253 { exit 1 }
		$0 !~ /^[A-Za-z0-9.-]+$/ { exit 1 }
		$0 ~ /^\./ || $0 ~ /\.$/ || $0 ~ /\.\./ { exit 1 }
		{
			n = split($0, labels, ".")
			for (i = 1; i <= n; i++) {
				if (length(labels[i]) < 1 || length(labels[i]) > 63 ||
				    labels[i] !~ /^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$/) {
					exit 1
				}
			}
		}
	'
}

qf_valid_domain() {
	qf_valid_hostname "$1" || return 1
	case "$1" in
	*.*) ;;
	*) return 1 ;;
	esac
	qf_valid_ip "$1" && return 1
	return 0
}

qf_valid_node_name() {
	case "$1" in
	'' | *[!A-Za-z0-9-]* | -* | *-) return 1 ;;
	esac
	[ "${#1}" -le 63 ]
}

qf_valid_email() {
	printf '%s\n' "$1" | awk '
		$0 !~ /^[^[:space:]@]+@[^[:space:]@]+\.[^[:space:]@]+$/ { exit 1 }
	'
}

qf_valid_ipv4() {
	printf '%s\n' "$1" | awk '
		{
			if ($0 !~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) exit 1
			n = split($0, octets, "[.]")
			if (n != 4) exit 1
			for (i = 1; i <= 4; i++) {
				if (length(octets[i]) > 3 || octets[i] + 0 > 255) exit 1
			}
		}
	'
}

qf_valid_ipv6() {
	printf '%s\n' "$1" | awk '
		function ipv4(s, a, n, i) {
			n = split(s, a, "[.]")
			if (n != 4) return 0
			for (i = 1; i <= 4; i++) {
				if (a[i] !~ /^[0-9]+$/ || length(a[i]) > 3 || a[i] + 0 > 255) return 0
			}
			return 1
		}
		function groups(s, a, n, i, count) {
			if (s == "") return 0
			n = split(s, a, ":")
			count = 0
			for (i = 1; i <= n; i++) {
				if (a[i] == "") return -100
				if (index(a[i], ".")) {
					if (i != n || !ipv4(a[i])) return -100
					count += 2
				} else {
					if (length(a[i]) > 4 || a[i] !~ /^[0-9A-Fa-f]+$/) return -100
					count++
				}
			}
			return count
		}
		{
			s = $0
			if (s !~ /:/ || s ~ /[^0-9A-Fa-f:.]/) exit 1
			pos = index(s, "::")
			if (pos) {
				if (index(substr(s, pos + 2), "::")) exit 1
				left = substr(s, 1, pos - 1)
				right = substr(s, pos + 2)
				count = groups(left) + groups(right)
				if (count < 0 || count >= 8) exit 1
				exit 0
			}
			if (groups(s) != 8) exit 1
		}
	'
}

qf_valid_ip() {
	qf_valid_ipv4 "$1" || qf_valid_ipv6 "$1"
}

qf_valid_private_docker_subnet() {
	printf '%s\n' "$1" | awk '
		{
			if ($0 !~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+\/[0-9]+$/) exit 1
			n = split($0, cidr, "/")
			if (n != 2 || cidr[2] + 0 < 24 || cidr[2] + 0 > 29) exit 1
			if (split(cidr[1], octets, "[.]") != 4) exit 1
			for (i = 1; i <= 4; i++) {
				if (length(octets[i]) > 3 || octets[i] + 0 > 255) exit 1
			}
			private = octets[1] == 10 ||
			          (octets[1] == 172 && octets[2] >= 16 && octets[2] <= 31) ||
			          (octets[1] == 192 && octets[2] == 168)
			if (!private) exit 1
			address = (((octets[1] * 256) + octets[2]) * 256 + octets[3]) * 256 + octets[4]
			block = 2 ^ (32 - cidr[2])
			if (address % block != 0) exit 1
		}
	'
}

qf_url_for_ip() {
	case "$1" in
	*:*) printf 'https://[%s]' "$1" ;;
	*) printf 'https://%s' "$1" ;;
	esac
}

qf_valid_base_url() {
	case "$1" in
	https://?*) ;;
	*) return 1 ;;
	esac
	case "${1#https://}" in
	'' | */* | *'?'* | *'#'*) return 1 ;;
	esac
	return 0
}

qf_dotenv_write() {
	_qf_env_file=$1
	_qf_env_key=$2
	_qf_env_value=$3
	case $_qf_env_key in
	'' | *[!A-Z0-9_]*) return 1 ;;
	esac
	case $_qf_env_value in
	*'
'*) return 1 ;;
	esac
	_qf_env_escaped=$(printf '%s' "$_qf_env_value" | sed -e 's/\\/\\\\/g' -e "s/'/\\\\'/g") || return 1
	printf "%s='%s'\n" "$_qf_env_key" "$_qf_env_escaped" >>"$_qf_env_file"
}

qf_dotenv_get() {
	_qf_get_file=$1
	_qf_get_key=$2
	[ -r "$_qf_get_file" ] || return 1
	_qf_get_raw=$(awk -v key="$_qf_get_key" '
		index($0, key "=") == 1 {
			print substr($0, length(key) + 2)
			exit
		}
	' "$_qf_get_file")
	case $_qf_get_raw in
	\'*)
		case $_qf_get_raw in
		*\') ;;
		*) return 1 ;;
		esac
		_qf_get_raw=${_qf_get_raw#\'}
		_qf_get_raw=${_qf_get_raw%\'}
		printf '%s' "$_qf_get_raw" | sed -e "s/\\\\'/'/g" -e 's/\\\\/\\/g'
		;;
	\"*)
		case $_qf_get_raw in
		*\") ;;
		*) return 1 ;;
		esac
		_qf_get_raw=${_qf_get_raw#\"}
		_qf_get_raw=${_qf_get_raw%\"}
		printf '%s' "$_qf_get_raw"
		;;
	*) printf '%s' "$_qf_get_raw" ;;
	esac
}

qf_file_mode() {
	if stat -c '%a' "$1" >/dev/null 2>&1; then
		stat -c '%a' "$1"
	else
		stat -f '%Lp' "$1" 2>/dev/null
	fi
}

qf_tailscale_backend_from_status() {
	awk -F'"' '
		$2 == "BackendState" {
			print $4
			exit
		}
	'
}

qf_tailscale_dns_from_status() {
	awk -F'"' '
		$2 == "Self" {
			in_self = 1
			next
		}
		in_self && $2 == "DNSName" {
			name = $4
			sub(/[.]$/, "", name)
			print name
			exit
		}
		in_self && $2 == "Peer" { exit }
	'
}

qf_scrub_bootstrap_credentials() {
	_qf_scrub_file=$1
	[ -f "$_qf_scrub_file" ] || return 1
	_qf_scrub_master_before=$(qf_dotenv_get "$_qf_scrub_file" QUEUEFRAME_MASTER_KEY) || return 1
	[ -n "$_qf_scrub_master_before" ] || return 1
	_qf_scrub_mode=$(qf_file_mode "$_qf_scrub_file" 2>/dev/null || true)
	case $_qf_scrub_mode in
	400 | 600) ;;
	*) _qf_scrub_mode=600 ;;
	esac
	_qf_scrub_temp=$(mktemp "${_qf_scrub_file}.seal.XXXXXX") || return 1
	if ! awk '
		$0 !~ /^(QUEUEFRAME_ADMIN_PASSWORD|TAILSCALE_AUTHKEY)=/
	' "$_qf_scrub_file" >"$_qf_scrub_temp"; then
		rm -f -- "$_qf_scrub_temp"
		return 1
	fi
	chmod "$_qf_scrub_mode" "$_qf_scrub_temp" || {
		rm -f -- "$_qf_scrub_temp"
		return 1
	}
	_qf_scrub_master_after=$(qf_dotenv_get "$_qf_scrub_temp" QUEUEFRAME_MASTER_KEY) || {
		rm -f -- "$_qf_scrub_temp"
		return 1
	}
	if [ "$_qf_scrub_master_before" != "$_qf_scrub_master_after" ]; then
		rm -f -- "$_qf_scrub_temp"
		return 1
	fi
	mv -- "$_qf_scrub_temp" "$_qf_scrub_file"
}
