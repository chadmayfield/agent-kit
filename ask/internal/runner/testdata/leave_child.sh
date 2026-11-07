#!/bin/sh
# Starts a background child that holds stdout, records "<own pid> <child pid>"
# in the file named by $1, and exits 0 without waiting.
sleep 30 &
echo "$$ $!" > "$1.tmp"
mv "$1.tmp" "$1"
exit 0
