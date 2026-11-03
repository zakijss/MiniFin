/* Software H.264 decoding with libavcodec, for when MI_VDEC cannot be used.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_VIDEO_SW_H
#define MINIFIN_VIDEO_SW_H
#include <libavcodec/avcodec.h>

#include "display.h"

typedef struct SwDec SwDec;

/* Takes ownership of ctx (from demux_codec_copy). NULL on failure. */
SwDec *swdec_open(AVCodecContext *ctx);
/* Feeds pkt (NULL to drain). 1 and *out set when a frame came out; *out
 * stays valid until the next call. pts in the packets' units. */
int swdec_decode(SwDec *s, AVPacket *pkt, AVFrame **out);
int64_t swdec_pts(const AVFrame *f);
int swdec_convert(SwDec *s, const AVFrame *f, Display *d);
/* 0 normal; 1 skip the loop filter; 2 also skip non-reference frames. */
void swdec_set_skip(SwDec *s, int level);
void swdec_flush(SwDec *s);
void swdec_close(SwDec *s);

#endif
