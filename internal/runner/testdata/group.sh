#!/bin/sh
# Writes its own pid, which is also the process group id, to the file named
# by $1 first thing. Then starts a background child, rewrites the file as
# "<own pid> <child pid>", and waits.
echo "$$" > "$1.tmp"
mv "$1.tmp" "$1"
sleep 300 &
echo "$$ $!" > "$1.tmp"
mv "$1.tmp" "$1"
wait
