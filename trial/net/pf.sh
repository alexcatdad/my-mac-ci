#!/bin/sh
# Load, unload or show the MMC pf anchor. Run with sudo. Runtime only: gone after a reboot.
set -eu
anchor=com.apple/050.mmc
token=/var/run/mmc-pf.token
dir=$(cd "$(dirname "$0")" && pwd)

case ${1:-} in
load)
  pfctl -n -f "$dir/mmc.pf.conf"                       # syntax check
  pfctl -a "$anchor" -f "$dir/mmc.pf.conf"
  [ -s "$token" ] || pfctl -E 2>&1 | sed -n 's/^Token : //p' > "$token"   # keep pf enabled while loaded
  pfctl -a "$anchor" -s rules
  ;;
unload)
  pfctl -a "$anchor" -F all
  if [ -s "$token" ]; then pfctl -X "$(cat "$token")"; rm -f "$token"; fi
  ;;
status)
  pfctl -s info | head -1
  pfctl -a "$anchor" -s rules
  ;;
*)
  echo "usage: sudo sh $0 load|unload|status" >&2
  exit 2
  ;;
esac
