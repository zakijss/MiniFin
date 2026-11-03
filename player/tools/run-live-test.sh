#!/bin/sh
# Runs tests/live_demux.c in a Debian Buster container (its FFmpeg 4.1 still
# has the FFmpeg 2.4 calls demux.c uses) against tools/test-relay.py in a
# second container. MEDIA is a directory with long480p30.ts in it.
#   MEDIA=/path/to/clips tools/run-live-test.sh
set -eu
cd "$(dirname "$0")/.."
: "${MEDIA:?set MEDIA to a directory with long480p30.ts}"
docker network create minifin-test >/dev/null 2>&1 || true
docker rm -f minifin-test-relay >/dev/null 2>&1 || true
docker run -d --name minifin-test-relay --network minifin-test -v "$MEDIA:/m:ro" -v "$PWD/tools:/t:ro" \
    minifin-relay python3 -I /t/test-relay.py /m 8000 >/dev/null
docker run --rm --network minifin-test -v "$PWD:/src" -v "$MEDIA:/m:ro" -w /src debian:buster sh -c '
  sed -i "s/deb\.debian\.org/archive.debian.org/g; s|security.debian.org|archive.debian.org/debian-security|g; /buster-updates/d" /etc/apt/sources.list
  apt-get -qq update >/dev/null && DEBIAN_FRONTEND=noninteractive apt-get -qq install -y gcc libavformat-dev libavcodec-dev >/dev/null
  gcc -std=gnu99 -O1 -Wall -Wno-deprecated-declarations -Isrc tests/live_demux.c src/demux.c src/urlutil.c src/log.c \
      -o /tmp/live_demux -lavformat -lavcodec -lavutil -lpthread -lm || exit 1
  sleep 1
  R=http://minifin-test-relay:8000
  echo "== live transcode, start 0, seek to 100 (stream begins at t)"
  /tmp/live_demux "$R/v/long480p30.ts?rate=300" 0 100 100 || exit 1
  echo "== live transcode resumed at 300, seek to 250"
  /tmp/live_demux "$R/v/long480p30.ts?t=300&rate=300" 300 250 250 || exit 1
  echo "== offline-style copy, seek to 100 (stream begins at the keyframe before)"
  /tmp/live_demux "$R/d/long480p30.ts" 0 100 98.4 || exit 1
  echo "== local file, seek to 100 (lands up to 3 s early, skips to 100)"
  /tmp/live_demux /m/long480p30.ts 0 100 97.5 || exit 1
'
docker rm -f minifin-test-relay >/dev/null
