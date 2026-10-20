#!/bin/sh
# Publish ftw.local for this device's LAN address through the host's
# avahi-daemon, so a browser on the LAN reaches the dashboard at
# http://ftw.local:8080 without the device's IP or balena UUID.
#
# The host keeps answering for its own <uuid>.local; this adds an address
# record beside it and changes nothing on the host. If another box on the LAN
# already holds ftw.local, avahi picks an alternative name and logs it.
set -u

NAME=ftw.local
INTERVAL=10

pid=
published=

stop_publisher() {
	if [ -n "$pid" ]; then
		kill "$pid" 2>/dev/null
		wait "$pid" 2>/dev/null
		pid=
	fi
}

trap 'stop_publisher; exit 0' TERM INT

lan_address() {
	# The default route's source address is the one LAN clients reach.
	addr=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p')
	if [ -z "$addr" ]; then
		# A LAN without a default route: the first physical interface.
		addr=$(ip -4 -o addr show scope global |
			awk '$2 ~ /^(eth|en|wlan|wl)/ { sub(/\/.*/, "", $4); print $4; exit }')
	fi
	printf '%s' "$addr"
}

while :; do
	addr=$(lan_address)
	if [ "$addr" != "$published" ] || { [ -n "$pid" ] && ! kill -0 "$pid" 2>/dev/null; }; then
		stop_publisher
		published=$addr
		if [ -n "$addr" ]; then
			echo "publishing $NAME -> $addr"
			# -R: the host already owns the reverse (PTR) record for its address.
			avahi-publish -a -R "$NAME" "$addr" &
			pid=$!
		else
			echo "no LAN address yet; $NAME not published"
		fi
	fi
	sleep "$INTERVAL" &
	wait $!
done
