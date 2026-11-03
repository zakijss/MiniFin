#!/bin/sh
# Copies the device's own shared libraries into build/sysroot/lib so the
# player links against exactly what it will load at run time. Nothing from
# here is shipped: the device already has these files.
#
#   tools/import-device-libs.sh [ssh-host]      (default: miyoo)
#   tools/import-device-libs.sh --verify        (check build/sysroot only)
#
# Every file is fetched into a temporary directory and checked against the
# SHA-256 below before it replaces anything. libavcodec.so.56 (45 MB) is not
# in the list: copies of it over the device's Wi-Fi came back damaged, so
# tools/build-linkonly-avcodec.sh builds a link-only stand-in instead.
# A different Onion build has different files: update the sums only after checking where they came from.
set -eu
cd "$(dirname "$0")/.."
DEST=build/sysroot/lib
HOST=miyoo
VERIFY=0
for a in "$@"; do
    case "$a" in
        --verify) VERIFY=1 ;;
        -*) echo "unknown option $a" >&2; exit 2 ;;
        *) HOST=$a ;;
    esac
done

LIBS='/config/lib/libmi_sys.so 6e8ab2f6fd40c511f89d0f6fc29c8a57b462fe13cd5fc1a1a6b40ed1b89fee20
/config/lib/libmi_vdec.so a6c17d622a92d64cfed1005adc2d9271012f547ebab735d7a794b204285136bb
/config/lib/libmi_divp.so c381522e63106a964bff577da66e864e5180beff343a21ef8aba4a052c20b00b
/config/lib/libmi_gfx.so ecb2d75ce79afcc32691e6ca9da93ac7539a0bcde8463611e2a592acf66e6af1
/config/lib/libmi_common.so 1685d354394190826e63aa8830c6f1328b6037caa657da912b74f10f19f8457f
/config/lib/libcam_os_wrapper.so 4a7594ea01200f6f6d06073d6e76bb9eb776dff4a11bc1ede84b4143992058cc
/mnt/SDCARD/.tmp_update/lib/parasyte/libavformat.so.56 f3fd599d8cdba5f45b4bd0120c1b8332ec40041a1664730df22f50483bfad23b
/mnt/SDCARD/.tmp_update/lib/parasyte/libavutil.so.54 fc5cb87861d5fbf0ec2bc99d5316880e73f81371becb10631aba728ff49e700d
/mnt/SDCARD/.tmp_update/lib/parasyte/libswresample.so.1 33cd6ba660712b3b7c25b41a6cad71f10c94a1fbbff222742ecace10be8f3cc8
/mnt/SDCARD/.tmp_update/lib/parasyte/libswscale.so.3 92c05dec7386ce8c0d9b47c9df59234b16c066a10ef11254089699ecdb3814e6'

sha() { if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi; }

check_dir() { # dir -> 0 if every file matches
    bad=0
    echo "$LIBS" | while read -r path sum; do
        f="$1/$(basename "$path")"
        if [ ! -f "$f" ]; then echo "missing $f" >&2; exit 1; fi
        if [ "$(sha "$f")" != "$sum" ]; then echo "checksum mismatch: $f" >&2; exit 1; fi
    done || bad=1
    return $bad
}

links() { # unversioned names for the linker
    (cd "$1" && for f in *.so.*; do ln -sf "$f" "${f%%.so.*}.so"; done)
}

if [ "$VERIFY" = 1 ]; then
    check_dir "$DEST" && echo "device libraries ok"
    exit $?
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT INT TERM
# One tar stream over one connection. The device's tar does not always end
# the stream cleanly, so success is judged by the checksums, not tar's status.
paths=$(echo "$LIBS" | while read -r p s; do printf '%s ' "${p#/}"; done)
# shellcheck disable=SC2086
perl -e 'alarm shift; exec @ARGV' 300 ssh -o ConnectTimeout=10 "$HOST" "cd / && tar cf - $paths" 2>/dev/null | (cd "$TMP" && tar xf - 2>/dev/null) || true
mkdir -p "$TMP/flat"
echo "$LIBS" | while read -r p s; do cp "$TMP${p}" "$TMP/flat/" 2>/dev/null || true; done
check_dir "$TMP/flat"
mkdir -p "$DEST"
cp "$TMP"/flat/* "$DEST"/
links "$DEST"
echo "device libraries imported into $DEST"
