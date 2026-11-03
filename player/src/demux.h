/* Input: libavformat, packet queues and seeking.
 *
 * Timestamps: every queued packet's pts/dts are rewritten to microseconds
 * of absolute video time ("abs"): the --start offset plus the time since
 * the input's first timestamp. A seek in an HTTP input reopens the URL
 * with ?t=<abs seconds> (MiniFin's relay starts a new transcode there, and
 * that stream begins at t); a local file is seeked with libavformat.
 * Decoders must not use the AVStream's codec context, which goes away when
 * the input is reopened: demux_codec_copy() gives them their own.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_DEMUX_H
#define MINIFIN_DEMUX_H
#include <pthread.h>
#include <stdint.h>

#include <libavformat/avformat.h>

#define TS_NONE INT64_MIN

typedef struct PktNode {
    AVPacket pkt;
    int serial;
    struct PktNode *next;
} PktNode;

typedef struct {
    PktNode *head, *tail;
    int n;
    int64_t bytes;
    pthread_mutex_t m;
    pthread_cond_t c;
} PacketQueue;

void pq_init(PacketQueue *q);
void pq_put(PacketQueue *q, AVPacket *pkt, int serial); /* takes the packet */
/* 1: got one; 0: none within timeout_ms. */
int pq_get(PacketQueue *q, AVPacket *pkt, int *serial, int timeout_ms);
void pq_flush(PacketQueue *q);
int pq_count(PacketQueue *q);
void pq_destroy(PacketQueue *q);

typedef struct Demux Demux;

typedef struct {
    int has_video, has_audio;
    enum AVCodecID vcodec, acodec;
    int width, height;
    double sar;          /* pixel aspect ratio */
    double fps;          /* nominal, 0 when unknown */
    double duration;     /* abs time of the end, 0 when unknown */
    int reopen_seeks;    /* seeks reopen the URL (HTTP) */
    PacketQueue vq, aq;
} DemuxInfo;

/* interrupt(): non-zero makes blocking reads give up (quit). */
Demux *demux_open(const char *url, double start, int (*interrupt)(void), char *err, int errlen);
DemuxInfo *demux_info(Demux *d);
/* A decoder context for the video (or audio) stream, owned by the caller
 * (avcodec_close + av_free). NULL if there is no such stream. */
AVCodecContext *demux_codec_copy(Demux *d, int video);

/* The reader thread body; runs until demux_stop. */
void *demux_thread(void *arg);
void demux_stop(Demux *d);

/* Ask for a seek to abs time t. Thread-safe; a newer request replaces one
 * that has not started. */
void demux_seek(Demux *d, double t);

typedef struct {
    int serial;         /* packets with an older serial are stale */
    double skip_until;  /* drop output before this abs time (NAN: none) */
    int eof;            /* reader reached the end */
    int seeking;        /* a seek is pending or being carried out */
    int seek_failed;    /* the last seek could not be done (cleared on read) */
    double stalled_for; /* seconds the current read has waited, 0 if none */
    int error;          /* fatal (message in demux_error) */
} DemuxState;

void demux_state(Demux *d, DemuxState *s);
const char *demux_error(Demux *d);
void demux_close(Demux *d);

#endif
