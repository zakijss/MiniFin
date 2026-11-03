#!/bin/sh
# MiniFin: movies and shows from a Jellyfin server, over the Wi-Fi or your tailnet.
cd "$(dirname "$0")"
# Keep the log small: start afresh once it passes 64 KB (the previous one
# is kept as minifin.log.old).
sz=$( { wc -c < minifin.log; } 2>/dev/null); [ $((${sz:-0})) -gt 65536 ] && mv -f minifin.log minifin.log.old
# Keep the CPU at 1 GHz or more while the screen is open, so the first
# frames after a press aren't drawn at the idle 400 MHz; while a video
# plays MiniFin raises it to the stock maximum. The normal minimum comes
# back on exit. (Stock speeds only: this is not an overclock.)
CPUMIN=/sys/devices/system/cpu/cpu0/cpufreq/scaling_min_freq
# The stock minimum is saved once per boot (/tmp is RAM): if a launcher is
# ever killed before it restores it, the next one still knows the real one.
[ -s /tmp/minideck-cpumin ] || cat $CPUMIN > /tmp/minideck-cpumin 2>/dev/null
oldmin=$(cat /tmp/minideck-cpumin 2>/dev/null)
[ -n "$oldmin" ] && [ "$oldmin" -lt 1000000 ] && echo 1000000 > $CPUMIN 2>/dev/null
restore() { [ -n "$oldmin" ] && echo "$oldmin" > $CPUMIN 2>/dev/null; }
# also when the launcher itself is stopped (power off, MiniTop)
trap 'restore; exit 0' TERM HUP INT
./minifin 2>> minifin.log
restore
