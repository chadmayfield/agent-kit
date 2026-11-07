#!/bin/sh
# Starts a child that calls setsid, so it leaves the process group, and
# keeps stdout open. The child writes its pid to the file named by $1 after
# setsid. The fake waits up to 5 s for that file, prints a line and exits 0.
/usr/bin/perl -MPOSIX -e 'POSIX::setsid() or die "setsid: $!"; open(my $f, ">", "$ARGV[0].tmp") or die; print $f "$$"; close($f); rename("$ARGV[0].tmp", $ARGV[0]) or die; exec("sleep", "30")' "$1" &
i=0
while [ ! -s "$1" ]; do
	i=$((i + 1))
	if [ "$i" -gt 500 ]; then
		echo "escape.sh: the child never wrote its pid" >&2
		exit 1
	fi
	sleep 0.01
done
echo "leader output"
exit 0
