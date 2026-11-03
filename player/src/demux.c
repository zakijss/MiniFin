/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "demux.h"

#include <errno.h>
#include <fcntl.h>
#include <netdb.h>
#include <poll.h>
#include <sys/socket.h>
#include <math.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include "log.h"
#include "urlutil.h"

/* ---- packet queue ---- */

void pq_init(PacketQueue *q)
{
    memset(q, 0, sizeof *q);
    pthread_mutex_init(&q->m, NULL);
    pthread_cond_init(&q->c, NULL);
}

void pq_put(PacketQueue *q, AVPacket *pkt, int serial)
{
    PktNode *n = malloc(sizeof *n);
    if (!n || av_dup_packet(pkt) < 0) {
        free(n);
        av_free_packet(pkt);
        return;
    }
    n->pkt = *pkt;
    n->serial = serial;
    n->next = NULL;
    pthread_mutex_lock(&q->m);
    if (q->tail) q->tail->next = n;
    else q->head = n;
    q->tail = n;
    q->n++;
    q->bytes += pkt->size;
    pthread_cond_signal(&q->c);
    pthread_mutex_unlock(&q->m);
}

int pq_get(PacketQueue *q, AVPacket *pkt, int *serial, int timeout_ms)
{
    int got = 0;
    pthread_mutex_lock(&q->m);
    if (!q->head && timeout_ms > 0) {
        struct timespec ts;
        clock_gettime(CLOCK_REALTIME, &ts);
        ts.tv_nsec += (long)timeout_ms * 1000000L;
        ts.tv_sec += ts.tv_nsec / 1000000000L;
        ts.tv_nsec %= 1000000000L;
        while (!q->head)
            if (pthread_cond_timedwait(&q->c, &q->m, &ts) == ETIMEDOUT) break;
    }
    if (q->head) {
        PktNode *n = q->head;
        q->head = n->next;
        if (!q->head) q->tail = NULL;
        q->n--;
        q->bytes -= n->pkt.size;
        *pkt = n->pkt;
        if (serial) *serial = n->serial;
        free(n);
        got = 1;
    }
    pthread_mutex_unlock(&q->m);
    return got;
}

void pq_flush(PacketQueue *q)
{
    pthread_mutex_lock(&q->m);
    while (q->head) {
        PktNode *n = q->head;
        q->head = n->next;
        av_free_packet(&n->pkt);
        free(n);
    }
    q->tail = NULL;
    q->n = 0;
    q->bytes = 0;
    pthread_mutex_unlock(&q->m);
}

int pq_count(PacketQueue *q)
{
    int n;
    pthread_mutex_lock(&q->m);
    n = q->n;
    pthread_mutex_unlock(&q->m);
    return n;
}

void pq_destroy(PacketQueue *q)
{
    pq_flush(q);
    pthread_mutex_destroy(&q->m);
    pthread_cond_destroy(&q->c);
}

/* ---- demuxer ---- */

struct Demux {
    DemuxInfo info;
    char *url;
    AVFormatContext *fc;
    int vidx, aidx;
    double base;  /* abs time of the input's first timestamp */
    double first; /* the input's first timestamp, seconds */
    int (*interrupt)(void);
    pthread_mutex_t m;
    pthread_cond_t c;
    int stop;
    int serial;
    double skip_until;
    int eof, error, seek_failed, seeking;
    int seek_req;
    double seek_to;
    volatile double read_since; /* now_sec() when the current read began, 0 when idle */
    double stall_limit;
    char errmsg[160];
};

static int io_interrupt(void *opaque)
{
    Demux *d = opaque;
    if (d->stop) return 1;
    if (d->interrupt && d->interrupt()) return 1;
    /* a reopen for a newer seek should not wait for an old stalled read */
    if (d->seek_req && d->info.reopen_seeks) return 1;
    double since = d->read_since;
    if (since > 0 && now_sec() - since > d->stall_limit) return 1;
    return 0;
}

static double env_num(const char *name, double def)
{
    const char *e = getenv(name);
    return e && atof(e) > 0 ? atof(e) : def;
}

/* Opens url into d->fc and finds the streams. 0 or an AVERROR. */
static int open_input(Demux *d, const char *url, AVInputFormat *fmt, char *err, int errlen)
{
    AVFormatContext *fc = avformat_alloc_context();
    AVDictionary *opts = NULL;
    int r;
    if (!fc) return AVERROR(ENOMEM);
    fc->interrupt_callback.callback = io_interrupt;
    fc->interrupt_callback.opaque = d;
    /* A reopen after a seek already knows the format and the codecs: read
     * as little as it takes to find the streams again. */
    fc->probesize = fmt ? 256 * 1024 : 1024 * 1024;
    fc->max_analyze_duration = fmt ? AV_TIME_BASE / 2 : 2 * AV_TIME_BASE;
    av_dict_set(&opts, "user-agent", "MiniFin-player", 0); /* "user_agent" from FFmpeg 2.5 on */
    double saved = d->stall_limit;
    d->stall_limit = env_num("MINIFIN_OPEN_TIMEOUT", 30);
    d->read_since = now_sec();
    r = avformat_open_input(&fc, url, fmt, &opts);
    av_dict_free(&opts);
    if (r >= 0) {
        r = avformat_find_stream_info(fc, NULL);
        if (r < 0) {
            snprintf(err, (size_t)errlen, "cannot read stream information");
            avformat_close_input(&fc);
        }
    } else {
        char b[96];
        av_strerror(r, b, sizeof b);
        snprintf(err, (size_t)errlen, "cannot open input: %s", b);
    }
    d->read_since = 0;
    d->stall_limit = saved;
    if (r < 0) return r;
    d->fc = fc;
    d->vidx = av_find_best_stream(fc, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
    d->aidx = av_find_best_stream(fc, AVMEDIA_TYPE_AUDIO, -1, d->vidx, NULL, 0);
    if (d->vidx < 0) d->vidx = -1;
    if (d->aidx < 0) d->aidx = -1;
    for (unsigned i = 0; i < fc->nb_streams; i++)
        if ((int)i != d->vidx && (int)i != d->aidx) fc->streams[i]->discard = AVDISCARD_ALL;
    d->first = fc->start_time != AV_NOPTS_VALUE ? fc->start_time / (double)AV_TIME_BASE : 0;
    return 0;
}

Demux *demux_open(const char *url, double start, int (*interrupt)(void), char *err, int errlen)
{
    Demux *d = calloc(1, sizeof *d);
    if (!d) return NULL;
    pthread_mutex_init(&d->m, NULL);
    pthread_cond_init(&d->c, NULL);
    d->interrupt = interrupt;
    d->skip_until = NAN;
    d->stall_limit = env_num("MINIFIN_STALL_SECONDS", 60);
    d->url = strdup(url);
    d->base = start;
    pq_init(&d->info.vq);
    pq_init(&d->info.aq);
    av_register_all();
    avformat_network_init();
    if (!d->url || open_input(d, url, NULL, err, errlen) < 0) {
        pq_destroy(&d->info.vq);
        pq_destroy(&d->info.aq);
        free(d->url);
        free(d);
        return NULL;
    }
    AVFormatContext *fc = d->fc;
    DemuxInfo *i = &d->info;
    i->reopen_seeks = url_is_network(url);
    if (d->vidx >= 0) {
        AVStream *st = fc->streams[d->vidx];
        AVRational sar = av_guess_sample_aspect_ratio(fc, st, NULL);
        AVRational fr = st->avg_frame_rate.num ? st->avg_frame_rate : st->r_frame_rate;
        i->has_video = 1;
        i->vcodec = st->codec->codec_id;
        i->width = st->codec->width;
        i->height = st->codec->height;
        i->sar = sar.num && sar.den ? av_q2d(sar) : 1;
        i->fps = fr.num && fr.den ? av_q2d(fr) : 0;
        if (i->fps > 120) i->fps = 0;
    }
    if (d->aidx >= 0) {
        i->has_audio = 1;
        i->acodec = fc->streams[d->aidx]->codec->codec_id;
    }
    if (fc->duration != AV_NOPTS_VALUE && fc->duration > 0) i->duration = start + fc->duration / (double)AV_TIME_BASE;
    plog("demux: %s, %u streams, video %d (%dx%d %.2f fps) audio %d, first ts %.3f, ends at %.1f, %s", fc->iformat->name,
         fc->nb_streams, d->vidx, i->width, i->height, i->fps, d->aidx, d->first, i->duration,
         i->reopen_seeks ? "seeks reopen the URL" : (fc->pb && fc->pb->seekable) ? "seekable file" : "not seekable");
    return d;
}

DemuxInfo *demux_info(Demux *d) { return &d->info; }

AVCodecContext *demux_codec_copy(Demux *d, int video)
{
    int idx = video ? d->vidx : d->aidx;
    AVCodecContext *c;
    if (idx < 0) return NULL;
    c = avcodec_alloc_context3(NULL);
    if (!c) return NULL;
    if (avcodec_copy_context(c, d->fc->streams[idx]->codec) < 0) {
        av_free(c);
        return NULL;
    }
    c->pkt_timebase = (AVRational){ 1, 1000000 };
    return c;
}

/* Rewrites timestamps to abs microseconds. */
static void to_abs(Demux *d, AVPacket *p)
{
    AVRational tb = d->fc->streams[p->stream_index]->time_base;
    double s = av_q2d(tb);
    if (p->pts != AV_NOPTS_VALUE) p->pts = (int64_t)llround((p->pts * s - d->first + d->base) * 1e6);
    else p->pts = TS_NONE;
    if (p->dts != AV_NOPTS_VALUE) p->dts = (int64_t)llround((p->dts * s - d->first + d->base) * 1e6);
    else p->dts = TS_NONE;
    p->duration = p->duration > 0 ? (int)llround(p->duration * s * 1e6) : 0;
}

static int read_packet(Demux *d, AVPacket *pkt)
{
    for (;;) {
        int r;
        d->read_since = now_sec();
        r = av_read_frame(d->fc, pkt);
        d->read_since = 0;
        if (r < 0) return r;
        if (pkt->stream_index == d->vidx || pkt->stream_index == d->aidx) return 0;
        av_free_packet(pkt);
    }
}

static int queues_full(Demux *d)
{
    DemuxInfo *i = &d->info;
    int64_t bytes;
    int vn, an;
    pthread_mutex_lock(&i->vq.m);
    bytes = i->vq.bytes;
    vn = i->vq.n;
    pthread_mutex_unlock(&i->vq.m);
    pthread_mutex_lock(&i->aq.m);
    bytes += i->aq.bytes;
    an = i->aq.n;
    pthread_mutex_unlock(&i->aq.m);
    if (bytes > 4 * 1024 * 1024) return 1;
    /* about 3 s of each (30 fps video, 47 AAC frames a second) */
    return (d->vidx < 0 || vn > 90) && (d->aidx < 0 || an > 140);
}

/* GETs url just far enough to read the response headers, for MiniFin's
 * X-MiniFin-Start (where a ?t= stream really begins). 1 and *start set
 * when the header was there. Gives up after 10 s or on quit. */
static int http_probe_start(Demux *d, const char *url, double *start)
{
    char host[256], path[2048], req[2400], buf[8192], portstr[8];
    int port, fd = -1, got = 0;
    size_t have = 0;
    struct addrinfo hints, *ai = NULL;
    double t0 = now_sec();
    if (url_split_http(url, host, sizeof host, &port, path, sizeof path) < 0) return 0;
    memset(&hints, 0, sizeof hints);
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;
    snprintf(portstr, sizeof portstr, "%d", port);
    if (getaddrinfo(host, portstr, &hints, &ai) != 0 || !ai) return 0;
    fd = socket(ai->ai_family, SOCK_STREAM | SOCK_CLOEXEC | SOCK_NONBLOCK, 0);
    if (fd < 0) goto done;
    if (connect(fd, ai->ai_addr, ai->ai_addrlen) < 0 && errno != EINPROGRESS) goto done;
    snprintf(req, sizeof req, "GET %s HTTP/1.1\r\nHost: %s:%d\r\nUser-Agent: MiniFin-player\r\nConnection: close\r\n\r\n",
             path, host, port);
    size_t sent = 0, rl = strlen(req);
    while (have < sizeof buf - 1) {
        struct pollfd pf = { fd, (short)(sent < rl ? POLLOUT : POLLIN), 0 };
        if (d->stop || (d->interrupt && d->interrupt()) || now_sec() - t0 > 10) goto done;
        if (poll(&pf, 1, 100) <= 0) continue;
        if (sent < rl) {
            ssize_t n = send(fd, req + sent, rl - sent, MSG_NOSIGNAL);
            if (n <= 0) goto done;
            sent += (size_t)n;
            continue;
        }
        ssize_t n = recv(fd, buf + have, sizeof buf - 1 - have, 0);
        if (n <= 0) break;
        have += (size_t)n;
        buf[have] = 0;
        if (strstr(buf, "\r\n\r\n")) break;
    }
    buf[have] = 0;
    char *end = strstr(buf, "\r\n\r\n");
    if (end) end[2] = 0;
    got = http_header_number(buf, "X-MiniFin-Start", start);
done:
    if (fd >= 0) close(fd);
    freeaddrinfo(ai);
    return got;
}

/* 0 done, -1 failed, 1 superseded by a newer request. */
static int do_seek(Demux *d, double t)
{
    if (d->info.reopen_seeks) {
        char url[2048], err[160];
        AVFormatContext *old = d->fc;
        int ov = d->vidx, oa = d->aidx;
        double of = d->first, ob = d->base;
        double begins = t, t0 = now_sec();
        url_with_time(url, sizeof url, d->url, t);
        int known = http_probe_start(d, url, &begins);
        double t1 = now_sec();
        if (d->seek_req) return 1;
        d->fc = NULL;
        int ro = open_input(d, url, old->iformat, err, sizeof err);
        plog("demux: reopen: header %.0f ms, open %.0f ms", (t1 - t0) * 1000, (now_sec() - t1) * 1000);
        if (ro < 0) {
            plog("demux: reopen %s: %s", url, err);
            d->fc = old;
            d->vidx = ov;
            d->aidx = oa;
            d->first = of;
            d->base = ob;
            return d->seek_req ? 1 : -1;
        }
        avformat_close_input(&old);
        if ((ov >= 0) != (d->vidx >= 0) || (oa >= 0) != (d->aidx >= 0))
            plog("demux: reopened stream has different streams (video %d audio %d)", d->vidx, d->aidx);
        /* The stream begins at t, or (offline copies) at the keyframe up to
         * about 2 s before it, as the relay's X-MiniFin-Start says. */
        if (!(begins <= t + 0.001 && begins > t - 30)) begins = t;
        d->base = begins;
        plog("demux: reopened at %.3f (%s)", begins, known ? "X-MiniFin-Start" : "no header, assuming t");
        pthread_mutex_lock(&d->m);
        d->skip_until = begins < t - 0.04 ? t : NAN;
        pthread_mutex_unlock(&d->m);
        return 0;
    }
    /* local file: land a few seconds early (keyframe), then skip to t */
    double rel = t - d->base, from = rel - 3 < 0 ? 0 : rel - 3;
    int64_t ts = (int64_t)((from + d->first) * AV_TIME_BASE);
    int r = avformat_seek_file(d->fc, -1, INT64_MIN, ts, ts, 0);
    if (r < 0) r = av_seek_frame(d->fc, -1, ts, AVSEEK_FLAG_BACKWARD);
    if (r < 0) return -1;
    pthread_mutex_lock(&d->m);
    d->skip_until = t;
    pthread_mutex_unlock(&d->m);
    return 0;
}

void *demux_thread(void *arg)
{
    Demux *d = arg;
    DemuxInfo *i = &d->info;
    for (;;) {
        pthread_mutex_lock(&d->m);
        if (d->stop) {
            pthread_mutex_unlock(&d->m);
            break;
        }
        if (d->seek_req) {
            double t = d->seek_to;
            d->seek_req = 0;
            pthread_mutex_unlock(&d->m);
            double t0 = now_sec();
            int r = do_seek(d, t);
            plog("demux: seek to %.1f %s in %.0f ms", t, r == 0 ? "done" : r > 0 ? "superseded" : "failed",
                 (now_sec() - t0) * 1000);
            pthread_mutex_lock(&d->m);
            if (r == 0) {
                pq_flush(&i->vq);
                pq_flush(&i->aq);
                d->serial++;
                d->eof = 0;
            } else if (r < 0) {
                d->seek_failed = 1;
            }
            d->seeking = d->seek_req;
            pthread_mutex_unlock(&d->m);
            continue;
        }
        if (d->eof) {
            struct timespec ts;
            clock_gettime(CLOCK_REALTIME, &ts);
            ts.tv_nsec += 50000000L;
            if (ts.tv_nsec >= 1000000000L) {
                ts.tv_sec++;
                ts.tv_nsec -= 1000000000L;
            }
            pthread_cond_timedwait(&d->c, &d->m, &ts);
            pthread_mutex_unlock(&d->m);
            continue;
        }
        int serial = d->serial;
        pthread_mutex_unlock(&d->m);
        if (queues_full(d)) {
            usleep(10000);
            continue;
        }
        AVPacket pkt;
        int r = read_packet(d, &pkt);
        if (r < 0) {
            pthread_mutex_lock(&d->m);
            if (d->stop || (d->interrupt && d->interrupt())) {
                pthread_mutex_unlock(&d->m);
                break;
            }
            if (d->seek_req) { /* interrupted by a seek: carry it out */
                pthread_mutex_unlock(&d->m);
                continue;
            }
            if (r == AVERROR_EXIT) {
                snprintf(d->errmsg, sizeof d->errmsg, "network stalled for %.0f s", d->stall_limit);
                d->error = 1;
            } else if (r != AVERROR_EOF) {
                char b[96];
                av_strerror(r, b, sizeof b);
                plog("demux: read error: %s (treated as the end)", b);
            }
            d->eof = 1;
            plog("demux: end of input");
            pthread_mutex_unlock(&d->m);
            continue;
        }
        int video = pkt.stream_index == d->vidx;
        to_abs(d, &pkt);
        pq_put(video ? &i->vq : &i->aq, &pkt, serial);
    }
    return NULL;
}

void demux_stop(Demux *d)
{
    pthread_mutex_lock(&d->m);
    d->stop = 1;
    pthread_cond_broadcast(&d->c);
    pthread_mutex_unlock(&d->m);
}

void demux_seek(Demux *d, double t)
{
    pthread_mutex_lock(&d->m);
    d->seek_req = 1;
    d->seek_to = t;
    d->seeking = 1;
    pthread_cond_broadcast(&d->c);
    pthread_mutex_unlock(&d->m);
}

void demux_state(Demux *d, DemuxState *s)
{
    pthread_mutex_lock(&d->m);
    s->serial = d->serial;
    s->skip_until = d->skip_until;
    s->eof = d->eof;
    s->seeking = d->seeking;
    s->seek_failed = d->seek_failed;
    d->seek_failed = 0;
    s->error = d->error;
    double since = d->read_since;
    s->stalled_for = since > 0 ? now_sec() - since : 0;
    pthread_mutex_unlock(&d->m);
}

const char *demux_error(Demux *d) { return d->errmsg; }

void demux_close(Demux *d)
{
    if (!d) return;
    pq_destroy(&d->info.vq);
    pq_destroy(&d->info.aq);
    if (d->fc) avformat_close_input(&d->fc);
    free(d->url);
    free(d);
}
