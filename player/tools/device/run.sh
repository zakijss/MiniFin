#!/bin/sh
# Test launcher for minifin-player (run by Onion via /tmp/cmd_to_run.sh,
# see go.sh). Lives in /mnt/SDCARD/.minifin-dev on the device.
# Arguments come from ./args (one shell-quoted line); a watchdog kills the
# player after ./timeout seconds (default 60). Output: out.txt, err.txt.
cd /mnt/SDCARD/.minifin-dev
export LD_LIBRARY_PATH=/lib:/config/lib:/mnt/SDCARD/miyoo/lib:/mnt/SDCARD/.tmp_update/lib:/mnt/SDCARD/.tmp_update/lib/parasyte:/customer/lib
pgrep audioserver >/dev/null && [ -f /mnt/SDCARD/miyoo/lib/libpadsp.so ] && export LD_PRELOAD=/mnt/SDCARD/miyoo/lib/libpadsp.so
[ -f env ] && . ./env
T=$(cat timeout 2>/dev/null || echo 60)
eval "set -- $(cat args)"
date +%s.%N > started
./minifin-player "$@" > out.txt 2> err.txt &
pid=$!
echo $pid > pid
# SIGTERM lets the player release the decoder and the screen; SIGKILL only
# if that did not work (a kill -9 while the MI drivers are in a bad state
# can leave them stuck until a reboot).
( sleep "$T"; kill $pid 2>/dev/null && echo "watchdog stopped $pid" >> out.txt; sleep 5; kill -9 $pid 2>/dev/null && echo "watchdog killed $pid" >> out.txt ) &
w=$!
wait $pid
echo "exit $?" >> out.txt
kill $w 2>/dev/null
rm -f pid
