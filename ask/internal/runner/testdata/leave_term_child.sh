#!/bin/sh
# Like leave_child.sh, but the child ignores SIGTERM, so stopping it takes
# the full grace period.
trap '' TERM
sleep 30 &
echo "$$ $!" > "$1.tmp"
mv "$1.tmp" "$1"
exit 0
