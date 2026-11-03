/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "video_sw.h"

#include <libswscale/swscale.h>
#include <stdlib.h>

#include "demux.h"
#include "log.h"

struct SwDec {
    AVCodecContext *c;
    AVFrame *f;
    struct SwsContext *sws;
    int skip;
};

SwDec *swdec_open(AVCodecContext *ctx)
{
    SwDec *s;
    AVCodec *codec;
    AVDictionary *o = NULL;
    if (!ctx) return NULL;
    codec = avcodec_find_decoder(ctx->codec_id);
    s = calloc(1, sizeof *s);
    if (!codec || !s) goto fail;
    ctx->thread_count = 2;
    ctx->thread_type = FF_THREAD_FRAME | FF_THREAD_SLICE;
    ctx->flags2 |= CODEC_FLAG2_FAST;
    if (avcodec_open2(ctx, codec, &o) < 0) goto fail;
    s->c = ctx;
    s->f = av_frame_alloc();
    plog("swdec: %s, %d threads", codec->name, ctx->thread_count);
    return s;
fail:
    plog("swdec: cannot open the decoder");
    avcodec_close(ctx);
    av_free(ctx);
    free(s);
    return NULL;
}

int swdec_decode(SwDec *s, AVPacket *pkt, AVFrame **out)
{
    AVPacket empty;
    int got = 0, r;
    if (!pkt) {
        av_init_packet(&empty);
        empty.data = NULL;
        empty.size = 0;
        pkt = &empty;
    }
    av_frame_unref(s->f);
    r = avcodec_decode_video2(s->c, s->f, &got, pkt);
    if (r < 0) return r;
    if (!got) return 0;
    *out = s->f;
    return 1;
}

int64_t swdec_pts(const AVFrame *f)
{
    int64_t t = av_frame_get_best_effort_timestamp(f);
    if (t == AV_NOPTS_VALUE) t = f->pkt_pts;
    return t == AV_NOPTS_VALUE ? TS_NONE : t;
}

int swdec_convert(SwDec *s, const AVFrame *f, Display *d)
{
    if (display_hw(d)) {
        /* same size, YUV 4:2:2 with U and V swapped (MI_GFX's byte order);
         * the 2D engine scales and converts to RGB */
        int stride;
        uint8_t *yuv = display_yuv_buffer(d, f->width & ~1, f->height & ~1, &stride);
        if (!yuv) return -1;
        s->sws = sws_getCachedContext(s->sws, f->width, f->height, f->format, f->width & ~1, f->height & ~1,
                                      AV_PIX_FMT_YUYV422, SWS_POINT, NULL, NULL, NULL);
        if (!s->sws) return -1;
        const uint8_t *src[4] = { f->data[0], f->data[2], f->data[1], NULL }; /* swap U, V */
        int ss[4] = { f->linesize[0], f->linesize[2], f->linesize[1], 0 };
        uint8_t *dst[4] = { yuv, NULL, NULL, NULL };
        int ds[4] = { stride, 0, 0, 0 };
        sws_scale(s->sws, src, ss, 0, f->height, dst, ds);
        display_stage_written(d);
        return 0;
    }
    int stride, x, y, w, h;
    uint32_t *stage = display_stage(d, NULL, &stride);
    display_video_rect(d, &x, &y, &w, &h);
    if (!stage) return -1;
    s->sws = sws_getCachedContext(s->sws, f->width, f->height, f->format, w, h, AV_PIX_FMT_BGRA,
                                  SWS_FAST_BILINEAR, NULL, NULL, NULL);
    if (!s->sws) return -1;
    uint8_t *dst[4] = { (uint8_t *)stage, NULL, NULL, NULL };
    int ds[4] = { stride * 4, 0, 0, 0 };
    sws_scale(s->sws, (const uint8_t *const *)f->data, f->linesize, 0, f->height, dst, ds);
    return 0;
}

void swdec_set_skip(SwDec *s, int level)
{
    if (level == s->skip) return;
    s->skip = level;
    s->c->skip_loop_filter = level >= 1 ? AVDISCARD_ALL : AVDISCARD_DEFAULT;
    s->c->skip_frame = level >= 2 ? AVDISCARD_NONREF : AVDISCARD_DEFAULT;
    plog("swdec: skip level %d", level);
}

void swdec_flush(SwDec *s) { avcodec_flush_buffers(s->c); }

void swdec_close(SwDec *s)
{
    if (!s) return;
    av_frame_free(&s->f);
    sws_freeContext(s->sws);
    avcodec_close(s->c);
    av_free(s->c);
    free(s);
}
