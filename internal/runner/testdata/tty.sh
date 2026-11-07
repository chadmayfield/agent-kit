#!/bin/sh
# Tries to read from the controlling terminal. With none, it exits 7.
if ( : < /dev/tty ) 2>/dev/null; then
	echo "has tty"
	read -r line < /dev/tty
	exit 0
fi
echo "no tty"
exit 7
