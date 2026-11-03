# usage (on device): sh go.sh  -> launches run.sh through Onion if it is safe
cd /mnt/SDCARD/.minifin-dev
if [ -f /tmp/cmd_to_run.sh ]; then echo "busy: /tmp/cmd_to_run.sh exists"; exit 1; fi
for p in $(pidof retroarch ra32 minifin-player); do
  grep -q "^State:.*Z" /proc/$p/status 2>/dev/null || { echo "busy: something running ($p)"; exit 1; }
done
if ! pidof MainUI >/dev/null; then echo "busy: MainUI not running"; exit 1; fi
rm -f out.txt err.txt
echo "cd /mnt/SDCARD/.minifin-dev && ./run.sh" > /tmp/cmd_to_run.sh
kill -9 $(pidof MainUI)
echo launched
