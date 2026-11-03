#!/bin/sh
# t.sh NAME 'ARGS' TIMEOUT [ENV] -- then steps on stdin: "sleep S", "press K..", "shot F", "wait"
# Launches the player on the device through Onion, runs the steps, waits for
# it to exit, and saves out/err to $D/NAME.out / NAME.err.
D=${OUT:-$(dirname "$0")}; name=$1; args=$2; to=$3; env=${4:-}
printf '%s\n' "$args" | ssh miyoo 'cat > /mnt/SDCARD/.minifin-dev/args' 2>/dev/null
printf '%s\n' "$env" | ssh miyoo 'cat > /mnt/SDCARD/.minifin-dev/env' 2>/dev/null
ssh -n miyoo "echo $to > /mnt/SDCARD/.minifin-dev/timeout" 2>/dev/null
r=$(ssh -n miyoo 'sh /mnt/SDCARD/.minifin-dev/go.sh' 2>/dev/null)
echo "$name: $r"
[ "$r" = launched ] || exit 1
while read -r cmd rest; do
  case "$cmd" in
    sleep) sleep "$rest" ;;
    press) "${MMP:-mmp}" press $rest </dev/null ;;
    shot) perl -e 'alarm 40; exec @ARGV' "${MMP:-mmp}" shot "$D/$rest.png" >/dev/null </dev/null ;;
    sh) ssh -n miyoo "$rest" 2>/dev/null </dev/null ;;
    wait) ;;
  esac
done
i=0
while [ $i -lt $((to + 20)) ]; do
  if ssh -n miyoo 'grep -q "^exit" /mnt/SDCARD/.minifin-dev/out.txt 2>/dev/null' 2>/dev/null; then break; fi
  sleep 2; i=$((i + 2))
done
ssh -n miyoo 'cat /mnt/SDCARD/.minifin-dev/out.txt' 2>/dev/null > "$D/$name.out"
ssh -n miyoo 'cat /mnt/SDCARD/.minifin-dev/err.txt' 2>/dev/null > "$D/$name.err"
echo "--- $name.out (last 6)"; tail -6 "$D/$name.out"
