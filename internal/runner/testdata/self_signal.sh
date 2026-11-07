#!/bin/sh
# Ends by sending itself SIGTERM.
kill -TERM $$
sleep 5
exit 0
