#!/bin/bash
# Point public port 80 at http-hostfix or back at openpak-edge, atomically, and persist it.
# Runs on the box under the deploy lock.
#
#   switch.sh on     port 80 -> 21090 (http-hostfix)
#   switch.sh off    port 80 -> 21081 (openpak-edge, the path before prds#144): the rollback
set -euo pipefail
case "${1:-}" in
on) from=21081 to=21090 ;;
off) from=21090 to=21081 ;;
*) echo "usage: $0 on|off" >&2; exit 2 ;;
esac
exec 9>~/openpak-deploy.lock
flock 9
if [ "$to" = 21090 ] && ! systemctl --user is-active --quiet openpak-http-hostfix.service; then
	echo "http-hostfix is not running" >&2
	exit 1
fi
# The redirect changes the destination port before the INPUT chain, which ends in a REJECT:
# the target port must be accepted there (live and saved) before traffic is sent to it.
if ! sudo -n iptables -C INPUT -p tcp -m tcp --dport "$to" -j ACCEPT 2>/dev/null; then
	pos=$(sudo -n iptables -L INPUT --line-numbers -n | awk -v p="dpt:$from" '$0 ~ p && /ACCEPT/ {print $1; exit}')
	sudo -n iptables -I INPUT "${pos:-1}" -p tcp -m tcp --dport "$to" -j ACCEPT
	echo "INPUT now accepts $to"
fi
if ! sudo -n grep -q -- "-A INPUT -p tcp -m tcp --dport $to -j ACCEPT" /etc/iptables/rules.v4; then
	sudo -n sed -i "/^-A INPUT -p tcp -m tcp --dport $from -j ACCEPT\$/a -A INPUT -p tcp -m tcp --dport $to -j ACCEPT" /etc/iptables/rules.v4
fi
rules=$(sudo -n iptables -t nat -S PREROUTING | grep -- "--dport 80 -j REDIRECT --to-ports $from" || true)
if [ -z "$rules" ]; then
	echo "no port-80 -> $from rule: already switched $1"
	sudo -n iptables -t nat -S PREROUTING | grep -- "--dport 80 " || true
	exit 0
fi
sudo -n cp -p /etc/iptables/rules.v4 "/etc/iptables/rules.v4.before-hostfix-$1-$(date +%Y%m%dT%H%M%S)"
# One iptables-restore commit: every port-80 rule moves at once (delete old, add new).
{
	echo "*nat"
	while read -r r; do
		echo "${r/-A PREROUTING/-D PREROUTING}"
		echo "${r/--to-ports $from/--to-ports $to}"
	done <<<"$rules"
	echo "COMMIT"
} | sudo -n iptables-restore --noflush
# The saved rules get the same edit, line by line (not a full iptables-save, which would also
# capture chains other tools add at run time).
sudo -n sed -i "/--dport 80 -j REDIRECT --to-ports $from\$/s/--to-ports $from\$/--to-ports $to/" /etc/iptables/rules.v4
echo "saved rules:"
sudo -n grep -- "--dport 80 -j REDIRECT" /etc/iptables/rules.v4
echo "port 80 now:"
sudo -n iptables -t nat -S PREROUTING | grep -- "--dport 80 "
