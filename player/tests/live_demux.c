/* Integration test for demux.c against tools/test-relay.py (or any server
 * with MiniFin's ?t= contract). Built and run in a Linux container with a
 * system FFmpeg (tools/run-live-test.sh); not part of "make test".
 *
 *   live_demux URL_OR_FILE START SEEK_TO EXPECT_BASE
 * Reads a second of packets, seeks to SEEK_TO, and checks that the first
 * packets after the seek carry abs times near EXPECT_BASE and that
 * skip_until is set when the stream starts before SEEK_TO.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include <math.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <unistd.h>

#include "demux.h"

static int no_interrupt(void) { return 0; }

static int first_after(Demux *d, int serial, double *t)
{
    DemuxInfo *i = demux_info(d);
    for (int k = 0; k < 400; k++) {
        AVPacket p;
        int s;
        if (pq_get(&i->vq, &p, &s, 50)) {
            int ok = s == serial && p.pts != TS_NONE;
            if (ok) *t = p.pts / 1e6;
            av_free_packet(&p);
            if (ok) return 1;
        }
    }
    return 0;
}

int main(int argc, char **argv)
{
    char err[200];
    if (argc < 5) return 2;
    double start = atof(argv[2]), to = atof(argv[3]), expect = atof(argv[4]), t = NAN;
    Demux *d = demux_open(argv[1], start, no_interrupt, err, sizeof err);
    if (!d) { printf("FAIL open: %s\n", err); return 1; }
    DemuxInfo *i = demux_info(d);
    pthread_t th;
    pthread_create(&th, NULL, demux_thread, d);
    if (!first_after(d, 0, &t)) { printf("FAIL no packets\n"); return 1; }
    printf("first packet at %.3f (start %.3f), reopen_seeks %d, duration %.1f\n", t, start, i->reopen_seeks, i->duration);
    int bad = fabs(t - start) > 1.0;
    usleep(500000);
    demux_seek(d, to);
    DemuxState ds;
    for (int k = 0; k < 200; k++) { demux_state(d, &ds); if (ds.serial == 1 && !ds.seeking) break; usleep(50000); }
    if (ds.serial != 1) { printf("FAIL seek did not finish (failed=%d)\n", ds.seek_failed); return 1; }
    if (!first_after(d, 1, &t)) { printf("FAIL no packets after seek\n"); return 1; }
    printf("after seek to %.3f: first video packet at %.3f, skip_until %.3f\n", to, t, ds.skip_until);
    if (t < expect - 0.6 || t > expect + 3) bad = 1; /* the relay's video may start after its audio */
    if (expect < to - 0.04 && !(fabs(ds.skip_until - to) < 0.001)) bad = 1;
    if (expect >= to - 0.04 && i->reopen_seeks && !isnan(ds.skip_until)) bad = 1;
    demux_stop(d);
    pthread_join(th, NULL);
    demux_close(d);
    printf(bad ? "FAIL\n" : "ok\n");
    return bad;
}
