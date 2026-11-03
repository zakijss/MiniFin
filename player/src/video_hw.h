/* H.264/H.265 decoding with the SSD202D's video decoder (MI_VDEC).
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_VIDEO_HW_H
#define MINIFIN_VIDEO_HW_H
#include <stdint.h>

#include "display.h"
#include "mi_sys.h"

typedef struct Vdec Vdec;

typedef struct {
    MI_SYS_BUF_HANDLE h;
    MI_SYS_BufInfo_t info;
    uint64_t pts; /* as sent with the stream */
    int valid;
} HwFrame;

/* Needs MI_SYS_Init done. NULL when the decoder cannot be opened. */
Vdec *vdec_open(int hevc, int width, int height);
/* 0 sent, 1 decoder input full (try again later), -1 error. */
int vdec_send(Vdec *v, const uint8_t *data, int len, uint64_t pts);
void vdec_send_eos(Vdec *v);
/* 1 and *f filled when a decoded frame is ready. */
int vdec_get(Vdec *v, HwFrame *f);
void vdec_put(Vdec *v, HwFrame *f);
/* Picture size of f after vdec_convert's decimation (sources wider than
 * 640 or taller than 480 are reduced by a whole factor). */
void vdec_frame_size(const HwFrame *f, int *w, int *h);
/* Converts f (NV12) into dst as YUV 4:2:2 in the byte order Y0 V Y1 U,
 * which MI_GFX reads as its YUV422 format. dst must hold
 * dst_stride * h bytes for the size vdec_frame_size gives. */
int vdec_convert(Vdec *v, HwFrame *f, uint8_t *dst, int dst_stride);
/* Drops everything queued (after a seek) by destroying and recreating the
 * channel. MI_VDEC_ResetChn is never used: on this firmware it leaves the
 * channel dead, and the kernel then hangs in its cleanup when the process
 * exits. Returns 0, or -1 if the channel could not be recreated (the
 * decoder is then closed and vdec_alive() is 0). */
int vdec_reset(Vdec *v);
int vdec_alive(const Vdec *v);
/* Frames the output queue holds; at most this many minus one may be in
 * flight. */
int vdec_queue(const Vdec *v);
void vdec_close(Vdec *v);
/* Best effort from a signal handler. */
void vdec_emergency_close(void);
/* Decoder statistics for the log. */
void vdec_stats(Vdec *v, unsigned *received, unsigned *decoded, int *err);

#endif
