#!/bin/sh
# Probe what a job container on the mmc network can reach. Run as the owner (no sudo).
# usage: sh probe.sh <zot-tailnet-ip> <lan-registry-ip> <lan-registry-port> <mac-lan-ip>
set -eu
[ $# -eq 4 ] || { echo "usage: sh $0 <zot-tailnet-ip> <lan-registry-ip> <lan-registry-port> <mac-lan-ip>" >&2; exit 2; }
port=18765
image=${MMC_IMAGE:-mmc-trial:2.338.2}

# A listener on the Mac, so "blocked" is not confused with "nothing listening".
python3 -m http.server "$port" --bind 0.0.0.0 >/dev/null 2>&1 &
listener=$!
trap 'kill $listener 2>/dev/null' EXIT
sleep 1

container run --rm --network mmc --dns 1.1.1.1 --kernel-arg ipv6.disable=1 --entrypoint /bin/bash "$image" -c '
# TCP connect test: open = handshake completed within 5 s.
t() { if timeout 5 bash -c "exec 3<>/dev/tcp/$3/$4" 2>/dev/null; then got=open; else got=blocked; fi
      [ "$got" = "$2" ] && r=PASS || r=FAIL
      printf "%-4s %-22s want=%-7s got=%s\n" "$r" "$1" "$2" "$got"; }
t "internet by name"    open    github.com      443
t "internet by IP"      open    1.1.1.1         443
t "zot over tailnet"    blocked "$0"            443
t "registry on LAN"     blocked "$1"            "$2"
t "Mac via job gateway" blocked 192.168.200.1   '"$port"'
t "Mac via LAN address" blocked "$3"            '"$port"'
t "Tailscale resolver"  blocked 100.100.100.100 53
[ -e /proc/net/if_inet6 ] && echo "FAIL IPv6 present" || echo "PASS IPv6 absent"
' "$@" 2>&1 | grep -v '^\['
