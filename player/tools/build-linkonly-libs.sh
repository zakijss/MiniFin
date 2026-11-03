#!/bin/sh
# Link-only stand-ins for the device libraries in build/sysroot/lib, for a
# build without the device (CI, or when its copies are not at hand). The
# player links against them, then loads the device's own libraries at run
# time; none of these is ever copied to the device. Only files that are
# missing are made, so the device's own copies win when present.
#
#  - FFmpeg 2.4.14 (libavformat, libavcodec, libavutil, libswresample,
#    libswscale), built from the official tarball (checksum checked by
#    fetch-deps.sh): the same sonames and symbol versions as Onion's build.
#  - The SigmaStar MI libraries: empty functions with the device's sonames,
#    named after the ones the player and probe call. No SDK code.
#
# Run inside the toolchain container.
set -eu
cd "$(dirname "$0")/.."
B=$(pwd)/build
L=$B/sysroot/lib
CROSS=${CROSS:-arm-linux-gnueabihf-}
FF=ffmpeg-2.4.14
mkdir -p "$L"

MI='libmi_sys MI_SYS_BindChnPort MI_SYS_ChnOutputPortGetBuf MI_SYS_ChnOutputPortPutBuf MI_SYS_Exit MI_SYS_FlushInvCache MI_SYS_GetVersion MI_SYS_Init MI_SYS_MMA_Alloc MI_SYS_MMA_Free MI_SYS_MemsetPa MI_SYS_Mmap MI_SYS_Munmap MI_SYS_SetChnOutputPortDepth MI_SYS_UnBindChnPort
libmi_vdec MI_VDEC_CreateChn MI_VDEC_DeInitDev MI_VDEC_DestroyChn MI_VDEC_GetChnParam MI_VDEC_GetChnStat MI_VDEC_InitDev MI_VDEC_SendStream MI_VDEC_SetChnParam MI_VDEC_SetDisplayMode MI_VDEC_SetOutputPortLayoutMode MI_VDEC_StartChn MI_VDEC_StopChn
libmi_divp MI_DIVP_CreateChn MI_DIVP_DestroyChn MI_DIVP_SetOutputPortAttr MI_DIVP_StartChn MI_DIVP_StopChn MI_DIVP_StretchBuf
libmi_gfx MI_GFX_BitBlit MI_GFX_Close MI_GFX_Open MI_GFX_QuickFill MI_GFX_WaitAllDone
libmi_common'
echo "$MI" | while read -r lib syms; do
    [ -f "$L/$lib.so" ] && continue
    for s in $syms; do echo "void $s(void) {}"; done > "$B/$lib-stub.c"
    "${CROSS}gcc" -shared -fPIC -nostdlib -Wl,-soname,"$lib.so" "$B/$lib-stub.c" -o "$L/$lib.so"
    rm -f "$B/$lib-stub.c"
    echo "link-only $lib.so"
done

FFLIBS='libavformat.so.56 libavcodec.so.56 libavutil.so.54 libswresample.so.1 libswscale.so.3'
missing=
for f in $FFLIBS; do [ -f "$L/$f" ] || missing="$missing $f"; done
[ -n "$missing" ] || exit 0
[ -f "$B/dl/$FF.tar.xz" ] || sh tools/fetch-deps.sh
echo "736567ee95115876d7af7f99f2fa565417c351b748bdc05973618980da1df037  $B/dl/$FF.tar.xz" | sha256sum -c - >/dev/null
W=$B/linkonly
rm -rf "$W"
mkdir -p "$W"
tar -C "$W" -xf "$B/dl/$FF.tar.xz"
cd "$W/$FF"
./configure --enable-cross-compile --cross-prefix="$CROSS" --target-os=linux --arch=arm \
    --enable-gpl --enable-shared --disable-static --disable-programs --disable-doc --disable-yasm \
    --disable-everything --enable-decoder=h264,aac --enable-parser=h264,aac --enable-bsf=h264_mp4toannexb \
    --enable-demuxer=mpegts,mov --enable-protocol=file,http \
    --disable-avdevice --disable-avfilter --disable-postproc >/dev/null
for f in $missing; do
    d=${f%%.so.*}
    make -j"$(nproc)" "$d/$f" >/dev/null
    cp "$d/$f" "$L/$f"
    ln -sf "$f" "$L/$d.so"
    echo "link-only $f"
done
rm -rf "$W"
