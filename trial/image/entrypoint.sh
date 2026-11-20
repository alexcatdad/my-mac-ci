#!/bin/sh
set -eu
umask 077
ulimit -c 0
ram=/run/mmc
# Refuse a disk-backed substitute or a guest that could swap out tmpfs pages.
if [ "$(stat -f -c %T "$ram" 2>/dev/null)" != tmpfs ] ||
    ! awk 'END { exit (NR != 1) }' /proc/swaps; then
    echo "runner requires a RAM mount and disabled guest swap" >&2
    exit 1
fi
root=$(mktemp -d "$ram/runner.XXXXXXXX")
cd "$root"
cp -R /home/runner/bin .
ln -s /home/runner/externals externals
mkdir -p /home/runner/_work
ln -s /home/runner/_work _work
mmc-jit-files
exec ./bin/Runner.Listener run
