/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "player.h"

#include <math.h>
#include <poll.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <sys/resource.h>
#include <unistd.h>

#include <libavcodec/avcodec.h>
#include <libavutil/opt.h>
#include <libswresample/swresample.h>

#include "audio.h"
#include "clock.h"
#include "demux.h"
#include "display.h"
#include "input.h"
#include "log.h"
#include "mi_sys.h"
#include "osd.h"
#include "srt.h"
#include "video_hw.h"
#include "video_sw.h"

#define PTS_BIAS 100000000LL /* VDEC timestamps are unsigned: abs us + 100 s */

typedef struct {
    Args a;
    Demux *dm;
    DemuxInfo *di;
    Display *disp;
    Vdec *vdec;
    SwDec *sw;
    AVCodecContext *vctx; /* for the mp4 -> Annex B filter (hw path) */
    AOut *ao;
    double fdur;

    /* clock */
    pthread_mutex_t clk_m;
    int clk_serial;
    int a_valid;         /* audio written since the last seek */
    double a_end;        /* abs time of the end of what was written */
    double a_last_write; /* now_sec() */
    Clock ext;           /* runs on wall time when there is no audio clock */

    volatile int paused;
    volatile int quit;
    volatile int volume; /* percent */
    volatile int video_finished, audio_finished;
    volatile int shown_serial; /* serial of the last frame on screen */
    volatile double last_shown;
    volatile int first_frame;
    volatile int fatal; /* the decoder died: stop with an error */

    pthread_mutex_t osd_m;
    OsdState osd;
    volatile int osd_dirty;
    double last_present;

    /* stats */
    long shown, dropped, decoded;
    DiffStats av;
    double max_late;
} P;

static P *g_p;
static volatile int g_quit_req;

void player_request_quit(void) { g_quit_req = 1; }

void player_emergency_cleanup(void)
{
    vdec_emergency_close();
    display_emergency_blank();
}

static int interrupt_cb(void) { return g_quit_req || (g_p && g_p->quit); }

/* ---- clock ---- */

static void clock_new_serial(P *p, int serial)
{
    pthread_mutex_lock(&p->clk_m);
    if (serial != p->clk_serial) {
        int paused = p->ext.paused;
        p->clk_serial = serial;
        p->a_valid = 0;
        clock_reset(&p->ext);
        p->ext.paused = paused;
    }
    pthread_mutex_unlock(&p->clk_m);
}

/* The time being heard (or, without audio, the wall-clock time line). */
static double master_clock(P *p, double now)
{
    double c;
    pthread_mutex_lock(&p->clk_m);
    /* While the audio is starved (buffering) the clock stops with it, so
     * video does not run ahead; only after 5 s without sound (an audio
     * track that ended early) does the wall clock take over. */
    if (p->ao && p->a_valid && !p->audio_finished && (p->paused || now - p->a_last_write < 5.0)) {
        c = p->a_end - aout_queued(p->ao);
        int paused = p->ext.paused;
        clock_set(&p->ext, c, now);
        p->ext.paused = paused;
    } else {
        c = clock_get(&p->ext, now);
    }
    pthread_mutex_unlock(&p->clk_m);
    return c;
}

/* Starts the wall clock at t if nothing drives the clock yet. */
static void clock_start_at(P *p, double t, double now)
{
    pthread_mutex_lock(&p->clk_m);
    if (!p->ext.valid && !p->a_valid) {
        int paused = p->ext.paused;
        clock_set(&p->ext, t, now);
        p->ext.paused = paused;
    }
    pthread_mutex_unlock(&p->clk_m);
}

static void set_paused(P *p, int paused)
{
    pthread_mutex_lock(&p->clk_m);
    p->paused = paused;
    clock_pause(&p->ext, paused, now_sec());
    pthread_mutex_unlock(&p->clk_m);
}

/* ---- screen ---- */

static void present(P *p)
{
    if (p->osd_dirty) {
        OsdState st;
        OsdRect r[OSD_MAX_RECTS];
        pthread_mutex_lock(&p->osd_m);
        st = p->osd;
        p->osd_dirty = 0;
        pthread_mutex_unlock(&p->osd_m);
        int n = osd_render(display_osd(p->disp), &st, r);
        display_osd_changed(p->disp, r, n);
    }
    display_present(p->disp);
    p->last_present = now_sec();
}

static void note_shown(P *p, double t, double clk, int serial)
{
    p->shown++;
    p->last_shown = t;
    p->shown_serial = serial;
    if (!isnan(clk) && !p->paused) {
        diff_add(&p->av, t - clk);
        if (clk - t > p->max_late) p->max_late = clk - t;
    }
    if (!p->first_frame) {
        p->first_frame = 1;
        plog("first frame on screen");
    }
}

/* ---- video, hardware ---- */

/* MI_SYS does not hold the decoder back when we keep frames: once its
 * output queue is full it overwrites the oldest. So only a few packets
 * (vdec_queue() - 1) are let into the decoder ahead of the frames we have
 * taken out. */

/* Decoded pictures, copied out of the decoder as YUV 4:2:2, waiting for
 * their time. The decoder hands frames over in decode order; they are
 * shown in timestamp order, holding back as many as the stream reorders. */
#define POOL 5
enum { SLOT_FREE, SLOT_READY, SLOT_SHOWN };
typedef struct {
    uint8_t *vir;
    uint64_t phys;
    size_t cap;
    int stride, w, h, state;
    int disabled; /* no MMA memory for it (720p leaves little) */
    double t;
} Slot;

static Slot *pool_free_slot(Slot *pool)
{
    for (int i = 0; i < POOL; i++)
        if (pool[i].state == SLOT_FREE && !pool[i].disabled) return &pool[i];
    return NULL;
}

/* Makes sure sl can hold need bytes. 0 or -1 (the slot is then disabled). */
static int slot_reserve(Slot *sl, size_t need)
{
    if (need <= sl->cap) return 0;
    display_mma_free(sl->vir, sl->phys, sl->cap);
    sl->cap = 0;
    sl->vir = display_mma_alloc(need, &sl->phys);
    if (!sl->vir) {
        sl->disabled = 1;
        return -1;
    }
    sl->cap = need;
    return 0;
}

static Slot *pool_next(Slot *pool, int *ready)
{
    Slot *best = NULL;
    *ready = 0;
    for (int i = 0; i < POOL; i++)
        if (pool[i].state == SLOT_READY) {
            (*ready)++;
            if (!best || pool[i].t < best->t) best = &pool[i];
        }
    return best;
}

static void *video_thread_hw(void *arg)
{
    P *p = arg;
    PacketQueue *vq = &p->di->vq;
    AVBitStreamFilterContext *bsf = NULL;
    AVPacket pend;
    Slot pool[POOL];
    int have_pend = 0, serial = -1, need_key = 1, eos_sent = 0, shown_in_serial = 0;
    int in_flight = 0, logged = 0, conv_fail = 0, stalls = 0;
    int reorder = p->vctx && p->vctx->has_b_frames > 0 ? p->vctx->has_b_frames : 1;
    long sends = 0, fulls = 0;
    double skip_until = NAN, eos_at = 0, last_out = now_sec(), last_show = 0, last_t = -1e9, conv_time = 0, last_nudge = 0;
    int vw = 0, vh = 0;
    /* buffer size for the stream's picture after decimation */
    int ek = (p->di->width + 639) / 640, ekh = (p->di->height + 479) / 480;
    if (ekh > ek) ek = ekh;
    if (ek < 1) ek = 1;
    size_t expect_size = (size_t)((((p->di->width / ek) & ~1) + 15) & ~15) * 2 * (size_t)((p->di->height / ek) & ~1);
    if (expect_size < 64 * 1024) expect_size = 640 * 2 * 480;

    int max_flight = vdec_queue(p->vdec) - 1;
    memset(pool, 0, sizeof pool);
    if (reorder > 3) reorder = 3;
    /* big pictures: only as many buffers as the reordering needs */
    if ((long)p->di->width * p->di->height > 640L * 480)
        for (int i = reorder + 2; i < POOL; i++) pool[i].disabled = 1;
    plog("video: holding %d frame(s) back for reordering", reorder);
    if (p->vctx && p->vctx->extradata_size > 0 && p->vctx->extradata[0] == 1) {
        bsf = av_bitstream_filter_init(p->vctx->codec_id == AV_CODEC_ID_HEVC ? "hevc_mp4toannexb" : "h264_mp4toannexb");
        plog("video: %s mp4 to Annex B filter", bsf ? "using" : "no");
    }
    while (!p->quit) {
        DemuxState ds;
        int busy = 0, ready;
        double now;
        demux_state(p->dm, &ds);
        if (ds.serial != serial) {
            if (have_pend) av_free_packet(&pend);
            have_pend = 0;
            if (serial >= 0) {
                HwFrame f;
                while (vdec_get(p->vdec, &f)) vdec_put(p->vdec, &f);
                if (vdec_reset(p->vdec) < 0) {
                    p->fatal = 1;
                    break;
                }
            }
            for (int i = 0; i < POOL; i++)
                if (pool[i].state == SLOT_READY) pool[i].state = SLOT_FREE; /* the one on screen stays */
            serial = ds.serial;
            skip_until = ds.skip_until;
            need_key = 1;
            eos_sent = 0;
            shown_in_serial = 0;
            in_flight = 0;
            last_t = -1e9;
            last_out = now_sec();
            p->video_finished = 0;
            clock_new_serial(p, serial);
        }
        if (p->paused) last_out = now_sec(); /* not a stall */
        /* feed the decoder, a few packets ahead of what came out */
        if (in_flight >= max_flight && now_sec() - last_nudge > 0.3 && now_sec() - last_out > 0.3) {
            in_flight--; /* the decoder dropped one (a damaged frame): let one more in */
            last_nudge = now_sec();
        }
        while (!have_pend && in_flight < max_flight) {
            int ps;
            if (!pq_get(vq, &pend, &ps, 0)) break;
            if (ps != serial || (need_key && !(pend.flags & AV_PKT_FLAG_KEY))) {
                av_free_packet(&pend);
                continue;
            }
            need_key = 0;
            have_pend = 1;
            if (bsf) {
                uint8_t *out = NULL;
                int outlen = 0;
                int r = av_bitstream_filter_filter(bsf, p->vctx, NULL, &out, &outlen, pend.data, pend.size,
                                                   pend.flags & AV_PKT_FLAG_KEY);
                if (r > 0 && out) {
                    AVPacket np;
                    av_init_packet(&np);
                    np.data = out;
                    np.size = outlen;
                    np.buf = av_buffer_create(out, (size_t)outlen, av_buffer_default_free, NULL, 0);
                    np.pts = pend.pts;
                    np.dts = pend.dts;
                    np.flags = pend.flags;
                    av_free_packet(&pend);
                    pend = np;
                }
            }
        }
        if (have_pend) {
            int64_t t = pend.pts != TS_NONE ? pend.pts : pend.dts;
            uint64_t vp = t != TS_NONE ? (uint64_t)(t + PTS_BIAS) : 0;
            int r = vdec_send(p->vdec, pend.data, pend.size, vp);
            if (r == 1) fulls++;
            if (r != 1) {
                if (r == 0) {
                    in_flight++;
                    sends++;
                }
                av_free_packet(&pend);
                have_pend = 0;
                busy = 1;
            }
        } else if (ds.eof && !eos_sent && pq_count(vq) == 0) {
            vdec_send_eos(p->vdec);
            eos_sent = 1;
            eos_at = now_sec();
        }
        /* copy decoded frames out while there is room; a slot gets its
         * memory before a frame is taken, so no frame is lost to a failed
         * allocation */
        for (;;) {
            Slot *sl = pool_free_slot(pool);
            HwFrame f;
            if (!sl) break;
            if (slot_reserve(sl, expect_size) < 0) {
                int usable = 0;
                for (int i = 0; i < POOL; i++) usable += !pool[i].disabled;
                plog("video: no MMA memory for another frame buffer, using %d", usable);
                if (usable < reorder + 2) {
                    p->fatal = 2;
                    break;
                }
                continue;
            }
            if (!vdec_get(p->vdec, &f)) break;
            p->decoded++;
            if (in_flight > 0) in_flight--;
            last_out = now_sec();
            stalls = 0;
            busy = 1;
            double t = ((int64_t)f.pts - PTS_BIAS) / 1e6;
            if (logged < 2) {
                MI_SYS_FrameData_t *fd = &f.info.stFrameData;
                logged++;
                plog("video: frame %ld pts %.3f type %d fmt %d %ux%u crop %ux%u stride %u", p->decoded, t,
                     f.info.eBufType, fd->ePixelFormat, fd->u16Width, fd->u16Height, fd->stContentCropWindow.u16Width,
                     fd->stContentCropWindow.u16Height, fd->u32Stride[0]);
            }
            if (!isnan(skip_until) && t < skip_until - 0.002) {
                vdec_put(p->vdec, &f);
                continue;
            }
            if (t <= last_t) { /* came after a later frame was shown: reorders more than we hold */
                vdec_put(p->vdec, &f);
                p->dropped++;
                if (reorder < 3) {
                    reorder++;
                    plog("video: out of order at %.3f, holding %d frame(s) back now", t, reorder);
                }
                continue;
            }
            int w, h;
            vdec_frame_size(&f, &w, &h);
            int stride = ((w + 15) & ~15) * 2;
            size_t need = (size_t)stride * (size_t)h;
            if (need > expect_size) expect_size = need;
            if (slot_reserve(sl, need) < 0) { /* bigger than expected and no memory */
                vdec_put(p->vdec, &f);
                p->dropped++;
                continue;
            }
            double tc = now_sec();
            int r = vdec_convert(p->vdec, &f, sl->vir, stride);
            conv_time += now_sec() - tc;
            vdec_put(p->vdec, &f);
            if (r < 0) {
                if (conv_fail++ < 3) plog("video: cannot use frame (type %d)", f.info.eBufType);
                continue;
            }
            MI_SYS_FlushInvCache(sl->vir, (MI_U32)need);
            sl->stride = stride;
            sl->w = w;
            sl->h = h;
            sl->t = t;
            sl->state = SLOT_READY;
        }
        /* the next picture, once enough are in to know the order */
        now = now_sec();
        Slot *nx = pool_next(pool, &ready);
        int drained = eos_sent && now - last_out > 0.15;
        if (nx && (ready > reorder || drained || (in_flight == 0 && pq_count(vq) == 0 && now - last_out > 0.15))) {
            double t = nx->t;
            if (nx->w != vw || nx->h != vh) {
                display_set_video(p->disp, nx->w, nx->h, p->di->sar);
                vw = nx->w;
                vh = nx->h;
            }
            if (!p->ao || !p->a_valid) clock_start_at(p, t, now);
            double clk = master_clock(p, now), w = 0;
            int act;
            if (!shown_in_serial && (p->paused || isnan(clk))) act = FRAME_SHOW; /* show where a seek landed */
            else act = frame_action(t, clk, p->fdur, ready > 1, &w);
            if (act == FRAME_DROP && now - last_show > 0.1) act = FRAME_SHOW; /* keep the picture moving */
            if (act == FRAME_SHOW) {
                for (int i = 0; i < POOL; i++)
                    if (pool[i].state == SLOT_SHOWN) pool[i].state = SLOT_FREE;
                nx->state = SLOT_SHOWN;
                display_set_source_yuv(p->disp, nx->phys, nx->stride, 0, 0, nx->w, nx->h);
                present(p);
                note_shown(p, t, clk, serial);
                shown_in_serial = 1;
                last_show = now;
                last_t = t;
                busy = 1;
            } else if (act == FRAME_DROP) {
                nx->state = SLOT_FREE;
                last_t = t;
                p->dropped++;
                busy = 1;
            } else if (!busy) {
                if (p->osd_dirty && now - p->last_present > 0.04) present(p);
                usleep((useconds_t)(w > 0.005 ? 5000 : w * 1e6 + 500));
                continue;
            }
        } else if (!nx && eos_sent && now - (eos_at > last_out ? eos_at : last_out) > 0.5) {
            p->video_finished = 1;
        }
        /* packets go in but nothing comes out. A stream that trickles in
         * from a slow server can leave the decoder holding its frames for
         * good: start a fresh channel at the next keyframe, as a seek does.
         * Three restarts in a row with nothing out (the decoder cannot get
         * memory) stop rather than show a frozen picture. */
        if (!p->paused && sends > 0 && now - last_out > 5 && !eos_sent && (have_pend || pq_count(vq) > 0)) {
            HwFrame f;
            if (++stalls > 3) {
                plog("video: the decoder gave nothing for 5 s, %d times", stalls - 1);
                p->fatal = 1;
                break;
            }
            plog("video: the decoder gave nothing for 5 s; restarting it at the next keyframe (%d)", stalls);
            if (have_pend) av_free_packet(&pend);
            have_pend = 0;
            while (vdec_get(p->vdec, &f)) vdec_put(p->vdec, &f);
            if (vdec_reset(p->vdec) < 0) {
                p->fatal = 1;
                break;
            }
            need_key = 1;
            in_flight = 0;
            last_out = now_sec();
            continue;
        }
        if (!busy) {
            if (p->osd_dirty && now - p->last_present > 0.04) present(p);
            usleep(2000);
        }
    }
    if (have_pend) av_free_packet(&pend);
    if (bsf) av_bitstream_filter_close(bsf);
    plog("video: sent %ld (decoder full %ld), conversion %.2f ms a frame", sends, fulls,
         p->decoded ? conv_time * 1000 / p->decoded : 0);
    /* the screen may still show a pool picture: blank it before freeing */
    display_set_source_yuv(p->disp, 0, 0, 0, 0, 0, 0);
    display_blank(p->disp);
    for (int i = 0; i < POOL; i++) display_mma_free(pool[i].vir, pool[i].phys, pool[i].cap);
    return NULL;
}

/* ---- video, software ---- */

static void *video_thread_sw(void *arg)
{
    P *p = arg;
    PacketQueue *vq = &p->di->vq;
    int serial = -1, need_key = 1, shown_in_serial = 0, draining = 0;
    double skip_until = NAN, last_show = 0;
    int vw = 0, vh = 0;
    while (!p->quit) {
        DemuxState ds;
        AVPacket pkt;
        AVFrame *f = NULL;
        int ps, got;
        demux_state(p->dm, &ds);
        if (ds.serial != serial) {
            if (serial >= 0) swdec_flush(p->sw);
            serial = ds.serial;
            skip_until = ds.skip_until;
            need_key = 1;
            shown_in_serial = 0;
            draining = 0;
            p->video_finished = 0;
            clock_new_serial(p, serial);
        }
        if (draining) {
            got = swdec_decode(p->sw, NULL, &f);
            if (got <= 0) {
                p->video_finished = 1;
                usleep(10000);
                if (p->osd_dirty && now_sec() - p->last_present > 0.04) present(p);
                continue;
            }
        } else {
            if (!pq_get(vq, &pkt, &ps, 10)) {
                if (ds.eof && pq_count(vq) == 0) draining = 1;
                if (p->osd_dirty && now_sec() - p->last_present > 0.04) present(p);
                continue;
            }
            if (ps != serial || (need_key && !(pkt.flags & AV_PKT_FLAG_KEY))) {
                av_free_packet(&pkt);
                continue;
            }
            need_key = 0;
            got = swdec_decode(p->sw, &pkt, &f);
            av_free_packet(&pkt);
            if (got <= 0) continue;
        }
        p->decoded++;
        int64_t ts = swdec_pts(f);
        double t = ts != TS_NONE ? ts / 1e6 : p->last_shown + p->fdur;
        if (!isnan(skip_until) && t < skip_until - 0.002) continue;
        if (f->width != vw || f->height != vh) {
            double sar = f->sample_aspect_ratio.num ? av_q2d(f->sample_aspect_ratio) : p->di->sar;
            display_set_video(p->disp, f->width, f->height, sar);
            vw = f->width;
            vh = f->height;
        }
        /* wait until it is due */
        for (;;) {
            double now = now_sec(), w = 0, clk;
            DemuxState d2;
            if (!p->ao || !p->a_valid) clock_start_at(p, t, now);
            clk = master_clock(p, now);
            int act = (!shown_in_serial && (p->paused || isnan(clk))) ? FRAME_SHOW
                                                                      : frame_action(t, clk, p->fdur, !draining, &w);
            if (act == FRAME_DROP && now - last_show > 0.25) act = FRAME_SHOW;
            if (act == FRAME_SHOW) {
                double late = isnan(clk) ? 0 : clk - t;
                swdec_set_skip(p->sw, late > 0.3 ? 2 : late > 0.08 ? 1 : late < 0.02 ? 0 : 1);
                swdec_convert(p->sw, f, p->disp);
                present(p);
                note_shown(p, t, clk, serial);
                shown_in_serial = 1;
                last_show = now;
                break;
            }
            if (act == FRAME_DROP) {
                p->dropped++;
                swdec_set_skip(p->sw, 2);
                break;
            }
            demux_state(p->dm, &d2);
            if (p->quit || d2.serial != serial) break;
            if (p->osd_dirty && now - p->last_present > 0.04) present(p);
            usleep((useconds_t)(w > 0.01 ? 10000 : w * 1e6 + 500));
        }
    }
    return NULL;
}

/* ---- audio ---- */

static void *audio_thread(void *arg)
{
    P *p = arg;
    PacketQueue *aq = &p->di->aq;
    AVCodecContext *c = NULL;
    SwrContext *swr = NULL;
    AVFrame *fr = av_frame_alloc();
    int serial = -1, in_rate = 0, in_fmt = -1, out_rate = p->ao ? aout_rate(p->ao) : 48000;
    int64_t in_layout = 0;
    double skip_until = NAN;
    int16_t *out = NULL;
    int out_cap = 0;

    if (p->ao) {
        c = demux_codec_copy(p->dm, 0);
        AVCodec *codec = c ? avcodec_find_decoder(c->codec_id) : NULL;
        if (!codec || avcodec_open2(c, codec, NULL) < 0) {
            plog("audio: cannot open the %s decoder", c ? avcodec_get_name(c->codec_id) : "audio");
            if (c) av_free(c);
            c = NULL;
        }
    }
    if (!c) p->audio_finished = 1;
    while (!p->quit) {
        DemuxState ds;
        AVPacket pkt;
        int ps;
        demux_state(p->dm, &ds);
        if (!c) { /* no sound: keep the queue empty */
            while (pq_get(aq, &pkt, &ps, 0)) av_free_packet(&pkt);
            usleep(20000);
            continue;
        }
        if (ds.serial != serial) {
            if (serial >= 0) avcodec_flush_buffers(c);
            serial = ds.serial;
            skip_until = ds.skip_until;
            p->audio_finished = 0;
            clock_new_serial(p, serial);
        }
        if (p->paused) {
            usleep(10000);
            continue;
        }
        if (!pq_get(aq, &pkt, &ps, 20)) {
            if (ds.eof && pq_count(aq) == 0 && !p->audio_finished) {
                p->audio_finished = 1;
                plog("audio: end");
            }
            continue;
        }
        if (ps != serial) {
            av_free_packet(&pkt);
            continue;
        }
        double t = pkt.pts != TS_NONE ? pkt.pts / 1e6 : NAN;
        AVPacket left = pkt;
        while (left.size > 0 && !p->quit) {
            int got = 0, used = avcodec_decode_audio4(c, fr, &got, &left);
            if (used < 0) break;
            left.data += used;
            left.size -= used;
            if (!got) continue;
            int64_t layout = fr->channel_layout ? (int64_t)fr->channel_layout : av_get_default_channel_layout(fr->channels);
            if (!swr || fr->sample_rate != in_rate || fr->format != in_fmt || layout != in_layout) {
                swr_free(&swr);
                swr = swr_alloc_set_opts(NULL, AV_CH_LAYOUT_STEREO, AV_SAMPLE_FMT_S16, out_rate, layout, fr->format,
                                         fr->sample_rate, 0, NULL);
                if (!swr || swr_init(swr) < 0) {
                    plog("audio: cannot convert %d Hz format %d", fr->sample_rate, fr->format);
                    swr_free(&swr);
                    break;
                }
                in_rate = fr->sample_rate;
                in_fmt = fr->format;
                in_layout = layout;
            }
            int max_out = (int)av_rescale_rnd(swr_get_delay(swr, in_rate) + fr->nb_samples, out_rate, in_rate, AV_ROUND_UP);
            if (max_out > out_cap) {
                free(out);
                out_cap = max_out + 256;
                out = malloc((size_t)out_cap * 4);
                if (!out) { out_cap = 0; break; }
            }
            uint8_t *o[1] = { (uint8_t *)out };
            int n = swr_convert(swr, o, out_cap, (const uint8_t **)fr->extended_data, fr->nb_samples);
            if (n <= 0) continue;
            if (isnan(t)) {
                pthread_mutex_lock(&p->clk_m);
                t = p->a_valid ? p->a_end : NAN;
                pthread_mutex_unlock(&p->clk_m);
                if (isnan(t)) continue;
            }
            double start = t;
            t += (double)fr->nb_samples / in_rate; /* next frame of this packet */
            int skip = 0;
            if (!isnan(skip_until)) {
                if (start + (double)n / out_rate <= skip_until) continue;
                if (start < skip_until) skip = (int)((skip_until - start) * out_rate);
                skip_until = NAN;
            }
            int vol = p->volume;
            if (vol != 100) {
                for (int i = skip * 2; i < n * 2; i++) {
                    int s = out[i] * vol / 100;
                    out[i] = (int16_t)(s > 32767 ? 32767 : s < -32768 ? -32768 : s);
                }
            }
            /* write in pieces so pause and seek are not held up */
            for (int k = skip; k < n && !p->quit;) {
                int m = n - k > 1024 ? 1024 : n - k;
                DemuxState d2;
                demux_state(p->dm, &d2);
                if (d2.serial != serial) break;
                while (p->paused && !p->quit) usleep(10000);
                if (aout_write(p->ao, out + k * 2, m) < 0) break;
                k += m;
                pthread_mutex_lock(&p->clk_m);
                if (p->clk_serial == serial) {
                    p->a_end = start + (double)k / out_rate;
                    p->a_valid = 1;
                    p->a_last_write = now_sec();
                }
                pthread_mutex_unlock(&p->clk_m);
            }
        }
        av_free_packet(&pkt);
    }
    swr_free(&swr);
    av_frame_free(&fr);
    free(out);
    if (c) {
        avcodec_close(c);
        av_free(c);
    }
    return NULL;
}

/* ---- main loop ---- */

static double rss_mb(void)
{
    FILE *f = fopen("/proc/self/statm", "r");
    long pages = 0, res = 0;
    if (f) {
        if (fscanf(f, "%ld %ld", &pages, &res) != 2) res = 0;
        fclose(f);
    }
    return res * (sysconf(_SC_PAGESIZE) / 1024.0) / 1024.0;
}

static double cpu_sec(void)
{
    struct rusage ru;
    getrusage(RUSAGE_SELF, &ru);
    return ru.ru_utime.tv_sec + ru.ru_utime.tv_usec / 1e6 + ru.ru_stime.tv_sec + ru.ru_stime.tv_usec / 1e6;
}

static void set_osd(P *p, const OsdState *st)
{
    pthread_mutex_lock(&p->osd_m);
    if (memcmp(&p->osd, st, sizeof *st) != 0) {
        p->osd = *st;
        p->osd_dirty = 1;
    }
    pthread_mutex_unlock(&p->osd_m);
}

int player_run(const Args *args)
{
    P *p = calloc(1, sizeof *p);
    char err[200] = "";
    int code = 0, input = -1;
    pthread_t th_demux, th_video, th_audio;
    int started = 0;
    Srt srt = { NULL, 0 };
    double t_start = now_sec();
    if (!p) return 1;
    g_p = p;
    p->a = *args;
    p->volume = 100;
    p->clk_serial = -1;
    pthread_mutex_init(&p->clk_m, NULL);
    pthread_mutex_init(&p->osd_m, NULL);
    clock_reset(&p->ext);

    const char *e = getenv("MINIFIN_GFX");
    p->disp = display_open(!(e && *e == '0'));
    if (!p->disp) {
        proto_error("cannot open the screen");
        free(p);
        return 1;
    }
    /* loading screen */
    OsdState st;
    memset(&st, 0, sizeof st);
    snprintf(st.title, sizeof st.title, "%s", args->title ? args->title : "");
    st.bar = 1;
    st.pos = args->start;
    st.dur = args->duration;
    st.volume = 100;
    snprintf(st.message, sizeof st.message, "Loading...");
    set_osd(p, &st);
    present(p);

    int subs_on = 0, has_subs = 0;
    char sub_msg[96] = "";
    if (args->sub) {
        int n = srt_load(args->sub, &srt);
        if (n > 0) {
            has_subs = subs_on = 1;
            plog("subtitles: %d cues from %s", n, args->sub);
        } else {
            snprintf(sub_msg, sizeof sub_msg, "Subtitles could not be read");
            plog("subtitles: cannot read %s", args->sub);
        }
    }

    p->dm = demux_open(args->url, args->start, interrupt_cb, err, sizeof err);
    if (!p->dm) {
        if (g_quit_req) {
            proto_time("quit", args->start);
            code = 0;
        } else {
            proto_error("%s", err);
            code = 1;
        }
        goto out;
    }
    p->di = demux_info(p->dm);
    if (!p->di->has_video) {
        proto_error("no video stream");
        code = 1;
        goto out;
    }
    double dur = p->di->duration > 0 ? p->di->duration : args->duration;
    p->fdur = p->di->fps > 0 ? 1.0 / p->di->fps : 1.0 / 25;
    proto_time("dur", dur);

    /* decoder */
    e = getenv("MINIFIN_DECODER");
    int want_hw = !(e && !strcmp(e, "sw"));
    if (want_hw && display_hw(p->disp) &&
        (p->di->vcodec == AV_CODEC_ID_H264 || p->di->vcodec == AV_CODEC_ID_HEVC)) {
        p->vdec = vdec_open(p->di->vcodec == AV_CODEC_ID_HEVC, p->di->width, p->di->height);
        if (p->vdec) p->vctx = demux_codec_copy(p->dm, 1);
    }
    if (!p->vdec) {
        p->sw = swdec_open(demux_codec_copy(p->dm, 1));
        if (!p->sw) {
            proto_error("cannot decode %s video", avcodec_get_name(p->di->vcodec));
            code = 1;
            goto out;
        }
    }
    proto_word("decoder", p->vdec ? "hw" : "sw");
    display_set_video(p->disp, p->di->width, p->di->height, p->di->sar);
    if (p->di->has_audio) {
        p->ao = aout_open(48000);
        if (!p->ao) plog("audio: none, video runs on the system clock");
    }

    input = input_open();
    pthread_create(&th_demux, NULL, demux_thread, p->dm);
    pthread_create(&th_video, NULL, p->vdec ? video_thread_hw : video_thread_sw, p);
    pthread_create(&th_audio, NULL, audio_thread, p);
    started = 1;

    /* state of the main loop */
    int bar_pinned = 0, seek_pending = 0, reported_first = 0, seek_failed_shown = 0;
    double bar_until = 0, msg_until = 0, last_pos_line = 0, seek_tgt = 0, last_press = 0, requested = NAN;
    double last_repeat = 0, cpu0 = cpu_sec(), wall0 = now_sec(), last_stats = now_sec();
    double seek_delay = p->di->reopen_seeks ? 0.4 : 0.15;
    char message[96] = "";
    int sub_hint = 0, last_serial = 0;
    /* X / SELECT pick a track, L2 / R2 an episode; MiniFin restarts the video. */
    int n_audio = track_count(args->audio), n_subs = track_count(args->subs);
    int audio_pick = args->audio_cur, subs_pick = args->subs_cur;
    double pick_at = 0, ep_armed_at = 0;
    int ep_armed = 0; /* BTN_L2_ or BTN_R2_ pressed once */
    const char *want = NULL; /* "next" or "prev" */
    int tracks_chosen = 0;
    if (sub_msg[0]) {
        snprintf(message, sizeof message, "%s", sub_msg);
        msg_until = now_sec() + 3;
    }
    for (;;) {
        struct pollfd pfd = { input, POLLIN, 0 };
        poll(&pfd, input >= 0 ? 1 : 0, 30);
        double now = now_sec();
        DemuxState ds;
        demux_state(p->dm, &ds);

        /* where we are */
        double pos;
        if (seek_pending) pos = seek_tgt;
        else if (ds.seeking || (!isnan(requested) && p->shown_serial != ds.serial)) pos = requested;
        else {
            pos = master_clock(p, now);
            if (isnan(pos)) pos = p->first_frame ? p->last_shown : args->start;
        }
        if (dur > 0 && pos > dur) pos = dur;
        if (pos < 0) pos = 0;
        if (ds.serial != last_serial && p->shown_serial == ds.serial) {
            last_serial = ds.serial;
            requested = NAN;
        }

        /* buttons */
        int code_, value;
        while (input_next(input, &code_, &value)) {
            if (value == 0) continue;
            int repeatable = code_ == BTN_LEFT_ || code_ == BTN_RIGHT_ || code_ == BTN_L1_ || code_ == BTN_R1_ ||
                             code_ == BTN_UP_ || code_ == BTN_DOWN_;
            if (value == 2) {
                if (!repeatable || now - last_repeat < 0.15) continue;
                last_repeat = now;
            }
            double step = 0;
            switch (code_) {
            case BTN_A_:
                set_paused(p, !p->paused);
                proto_int("paused", p->paused);
                break;
            case BTN_B_:
            case BTN_MENU_:
                g_quit_req = 1;
                break;
            case BTN_LEFT_: step = -10; break;
            case BTN_RIGHT_: step = 10; break;
            case BTN_L1_: step = -60; break;
            case BTN_R1_: step = 60; break;
            case BTN_UP_:
            case BTN_DOWN_: {
                int v = p->volume + (code_ == BTN_UP_ ? 10 : -10);
                p->volume = v < 0 ? 0 : v > 150 ? 150 : v;
                snprintf(message, sizeof message, "Volume %d%%", p->volume);
                msg_until = now + 1.5;
                break;
            }
            case BTN_Y_:
                if (!has_subs) {
                    snprintf(message, sizeof message, "No subtitles");
                } else {
                    subs_on = !subs_on;
                    snprintf(message, sizeof message, "Subtitles %s", subs_on ? "on" : "off");
                }
                msg_until = now + 1.5;
                break;
            case BTN_X_:
            case BTN_SELECT_: {
                int is_audio = code_ == BTN_X_, n = is_audio ? n_audio : n_subs;
                int *pick = is_audio ? &audio_pick : &subs_pick;
                char name[64];
                if (n < 2) {
                    snprintf(message, sizeof message, is_audio ? "Only one audio track" : "No other subtitles");
                } else {
                    *pick = (*pick + 1) % n;
                    track_name(is_audio ? args->audio : args->subs, *pick, name, sizeof name);
                    snprintf(message, sizeof message, "%s: %s (%d of %d)", is_audio ? "Audio" : "Subtitles", name, *pick + 1, n);
                    pick_at = now;
                }
                msg_until = now + 1.5;
                break;
            }
            case BTN_L2_:
            case BTN_R2_: {
                const char *ep = code_ == BTN_R2_ ? args->next : args->prev;
                if (!ep) {
                    snprintf(message, sizeof message, "No %s episode", code_ == BTN_R2_ ? "next" : "previous");
                } else if (ep_armed == code_ && now - ep_armed_at < 3) {
                    want = code_ == BTN_R2_ ? "next" : "prev";
                    g_quit_req = 1;
                } else {
                    snprintf(message, sizeof message, "%s again: %s", code_ == BTN_R2_ ? "R2" : "L2", ep); /* the end may be cut */
                    ep_armed = code_;
                    ep_armed_at = now;
                }
                msg_until = now + 3;
                break;
            }
            case BTN_START_:
                bar_pinned = !bar_pinned;
                if (!bar_pinned) bar_until = 0;
                break;
            default: break;
            }
            if (step != 0) {
                double from = seek_pending ? seek_tgt : pos;
                seek_tgt = seek_target(from, step, dur);
                seek_pending = 1;
                last_press = now;
                bar_until = now + 3;
            }
        }
        /* a track choice holds for 1.2 s, so X can step past one */
        if (pick_at && now - pick_at >= 1.2) {
            pick_at = 0;
            tracks_chosen = audio_pick != args->audio_cur || subs_pick != args->subs_cur;
            if (tracks_chosen) g_quit_req = 1;
        }
        if (g_quit_req) break;
        if (seek_pending && now - last_press >= seek_delay) {
            demux_seek(p->dm, seek_tgt);
            requested = seek_tgt;
            seek_pending = 0;
            seek_failed_shown = 0;
            bar_until = now + 3;
        }
        if (ds.seek_failed && !seek_failed_shown) {
            snprintf(message, sizeof message, "Can't seek here");
            msg_until = now + 2;
            seek_failed_shown = 1;
            requested = NAN;
        }

        /* protocol lines */
        if (p->first_frame && !reported_first) {
            reported_first = 1;
            plog("startup %.0f ms", (now - t_start) * 1000);
            proto_time("pos", pos);
            last_pos_line = now;
        } else if (reported_first && now - last_pos_line >= 1.0) {
            proto_time("pos", pos);
            last_pos_line = now;
        }
        if (p->fatal) {
            proto_error("%s", p->fatal == 2 ? "not enough video memory for this resolution" : "the video decoder stopped working");
            code = 1;
            break;
        }
        if (ds.error) {
            proto_error("%s", demux_error(p->dm));
            code = 1;
            break;
        }
        if (p->video_finished && p->audio_finished && ds.eof && !seek_pending && !ds.seeking) {
            double last = p->last_shown;
            if (dur > 0 && p->di->reopen_seeks && last < dur - 30) {
                proto_error("stream ended early at %.1f of %.1f s", last, dur);
                code = 1;
            } else {
                proto_bare("end");
            }
            break;
        }

        /* overlay */
        memset(&st, 0, sizeof st);
        snprintf(st.title, sizeof st.title, "%s", args->title ? args->title : "");
        st.pos = floor(pos);
        st.dur = dur;
        st.paused = p->paused;
        st.volume = p->volume;
        st.has_subs = has_subs;
        st.subs_on = subs_on;
        int seeking_now = seek_pending || ds.seeking || !isnan(requested);
        st.bar = bar_pinned || p->paused || now < bar_until || seeking_now || !p->first_frame;
        if (now < msg_until) {
            snprintf(st.message, sizeof st.message, "%s", message);
        } else if (seeking_now) {
            char tb[32];
            fmt_clock(tb, sizeof tb, seek_pending ? seek_tgt : requested);
            snprintf(st.message, sizeof st.message, "Seeking to %s", tb);
        } else if (!p->first_frame) {
            snprintf(st.message, sizeof st.message, "Loading...");
        } else if (!ds.eof && ds.stalled_for > 1.0 && pq_count(&p->di->vq) == 0) {
            snprintf(st.message, sizeof st.message, ds.stalled_for > 10 ? "Waiting for the network..." : "Buffering...");
        }
        if (subs_on && !seeking_now) srt_text_at(&srt, pos, &sub_hint, st.sub, sizeof st.sub);
        set_osd(p, &st);

        if (now - last_stats >= 10) {
            double c = cpu_sec();
            plog("stats: pos %.1f shown %ld dropped %ld decoded %ld, cpu %.0f%%, rss %.1f MB, av %.1f ms (sd %.1f, min %.1f max %.1f)",
                 pos, p->shown, p->dropped, p->decoded, 100 * (c - cpu0) / (now - wall0), rss_mb(),
                 diff_mean(&p->av) * 1000, diff_sd(&p->av) * 1000, p->av.min * 1000, p->av.max * 1000);
            cpu0 = c;
            wall0 = now;
            last_stats = now;
        }
    }
    if (g_quit_req && code == 0) {
        double pos = master_clock(p, now_sec());
        if (seek_pending) pos = seek_tgt;
        else if (!isnan(requested)) pos = requested;
        else if (isnan(pos)) pos = p->first_frame ? p->last_shown : args->start;
        if (tracks_chosen && audio_pick != args->audio_cur) proto_int("audio", audio_pick);
        if (tracks_chosen && subs_pick != args->subs_cur) proto_int("subtitle", subs_pick);
        if (want) proto_bare(want);
        proto_time("quit", pos < 0 ? 0 : pos);
    }
out:
    alarm(5); /* if anything below hangs, the SIGALRM handler cleans up */
    p->quit = 1;
    if (started) {
        demux_stop(p->dm);
        pthread_join(th_video, NULL);
        pthread_join(th_audio, NULL);
        pthread_join(th_demux, NULL);
        plog("played: shown %ld dropped %ld decoded %ld, av mean %.1f ms sd %.1f ms, worst late %.0f ms",
             p->shown, p->dropped, p->decoded, diff_mean(&p->av) * 1000, diff_sd(&p->av) * 1000, p->max_late * 1000);
    }
    if (p->vdec) {
        unsigned rx, dec;
        int er;
        vdec_stats(p->vdec, &rx, &dec, &er);
        plog("vdec: received %u decoded %u err %d", rx, dec, er);
    }
    vdec_close(p->vdec);
    swdec_close(p->sw);
    if (p->vctx) {
        avcodec_close(p->vctx);
        av_free(p->vctx);
    }
    aout_close(p->ao);
    int hw = display_hw(p->disp);
    display_close(p->disp);
    if (hw) MI_SYS_Exit();
    input_close(input);
    demux_close(p->dm);
    srt_free(&srt);
    g_p = NULL;
    free(p);
    alarm(0);
    return code;
}
