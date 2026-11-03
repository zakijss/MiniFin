#!/bin/sh
# Fetches the headers the player is compiled against into build/include and
# checks every download's SHA-256. Run inside the toolchain container (the
# FFmpeg step runs FFmpeg's configure with the cross compiler).
#
#  - FFmpeg 2.4.14 headers, from the official release tarball. Only
#    configure and "make install-headers" run; nothing is compiled. At run
#    time the player uses Onion's own FFmpeg 2.4.14 shared libraries.
#  - SigmaStar MI SDK headers (mi_sys, mi_vdec, mi_divp, mi_gfx) from
#    steward-fu/sdl2, "SDL v2.0 Ported for Miyoo Mini (Plus)", at a pinned
#    commit. They match the device's /config/lib/libmi_*.so.
set -eu
cd "$(dirname "$0")/.."
B=$(pwd)/build
DL=$B/dl
INC=$B/include
CROSS=${CROSS:-arm-linux-gnueabihf-}
mkdir -p "$DL" "$INC/mi"

fetch() { # url sha256 file
    if [ ! -f "$DL/$3" ] || ! echo "$2  $DL/$3" | sha256sum -c - >/dev/null 2>&1; then
        wget -q -O "$DL/$3.tmp" "$1"
        mv -f "$DL/$3.tmp" "$DL/$3"
    fi
    echo "$2  $DL/$3" | sha256sum -c - >/dev/null 2>&1 || {
        echo "fetch-deps: checksum mismatch for $3 ($1)" >&2
        rm -f "$DL/$3"
        exit 1
    }
}

SDL2_COMMIT=0631abc8e8916db6f9bc7e2afd0c22913d092a29
MI_HEADERS='mi_common_datatype.h 90ddd7556f441caa600fbec84ca97005671423f79794d31e13f99daba13b725f
mi_common.h e803aac95bfb7111d5a28056e82c7c35aeff2df2f5eebcc0decfbc5362052d77
mi_sys_datatype.h 6325f8ae893a74f75e1bfde86725f029a63de7a83934955c81ae57c610a7e2ac
mi_sys.h 31d43935d18ed269897ddd592ad1d12f1012da841c4b05fab2890105eef33f2d
mi_vdec_datatype.h 9b0e94ba9308dd024111daef30fb9825d6d05a75722385b80e226de11b2fba44
mi_vdec.h f8a61336d2885da3e94ae144010d3b8e955b3cf9b1fb68c52040cc51327bd292
mi_divp_datatype.h c13c844f204494123a7db21ed528e9634f1da2949004dbc02d55841c60af18bc
mi_divp.h 29cf850d0e147379985d8962f2541a460f1af364a361713fb0e1ac7c5818d93c
mi_gfx_datatype.h a065540ec8174f428002a718e7c70d4a53f5dd96f2352618d5135cce6002bb50
mi_gfx.h 06ccef2c1cfa24b9f8365088afdf556e046048f45f1c9c79d8919c61e78da16b'

echo "$MI_HEADERS" | while read -r f sum; do
    fetch "https://raw.githubusercontent.com/steward-fu/sdl2/$SDL2_COMMIT/mini/inc/$f" "$sum" "$f"
    cp "$DL/$f" "$INC/mi/$f"
done
echo "fetch-deps: MI headers ok (steward-fu/sdl2 @ $SDL2_COMMIT)"

FF=ffmpeg-2.4.14
fetch https://ffmpeg.org/releases/$FF.tar.xz \
    736567ee95115876d7af7f99f2fa565417c351b748bdc05973618980da1df037 $FF.tar.xz
if [ ! -f "$INC/libavcodec/avcodec.h" ]; then
    rm -rf "$B/$FF"
    tar -C "$B" -xf "$DL/$FF.tar.xz"
    # Same feature switches that matter for the public headers as Onion's
    # build (--enable-gpl --enable-nonfree with libx264); the headers do not
    # depend on which external libraries are enabled.
    (cd "$B/$FF" && ./configure --prefix="$B/ffprefix" --incdir="$INC" \
        --enable-cross-compile --cross-prefix="$CROSS" --target-os=linux --arch=arm \
        --enable-gpl --enable-nonfree --enable-shared --disable-static \
        --disable-programs --disable-doc --disable-yasm >/dev/null \
     && make install-headers >/dev/null)
    rm -rf "$B/$FF"
fi
echo "fetch-deps: FFmpeg 2.4.14 headers ok"
