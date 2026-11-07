#!/bin/sh
# Like leave_child.sh, but the child does not hold stdout or stderr.
sleep 30 >/dev/null 2>&1 &
echo "$$ $!" > "$1.tmp"
mv "$1.tmp" "$1"
exit 0
