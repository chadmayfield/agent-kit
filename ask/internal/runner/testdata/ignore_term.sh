#!/bin/sh
# Like group.sh, but the shell and its child both ignore SIGTERM.
trap '' TERM
sleep 300 &
echo "$$ $!" > "$1.tmp"
mv "$1.tmp" "$1"
wait
