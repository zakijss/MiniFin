/* A/V clock maths, kept free of device code so it can be tested anywhere.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_CLOCK_H
#define MINIFIN_CLOCK_H

/* A clock that runs with wall time from the last point it was set. */
typedef struct {
    double pts;   /* media time at 'at' */
    double at;    /* wall time when pts was set */
    int paused;
    int valid;
} Clock;

void clock_reset(Clock *c);
void clock_set(Clock *c, double pts, double now);
double clock_get(const Clock *c, double now);
void clock_pause(Clock *c, int paused, double now);

/* Media time now playing out of the speaker: the end of what was written
 * minus what is still queued in front of it. */
double audio_clock(double written_end, double queued_bytes, double bytes_per_sec, double fixed_latency);

/* What to do with the next video frame. */
enum { FRAME_WAIT, FRAME_SHOW, FRAME_DROP };

/* frame_pts and clock in seconds; frame_dur is the nominal frame time.
 * have_next tells whether another decoded frame is ready right after this
 * one (a late frame is only dropped when there is something to replace it).
 * *wait gets how long to sleep for FRAME_WAIT. */
int frame_action(double frame_pts, double clock, double frame_dur, int have_next, double *wait);

/* Seek target after a relative step, kept inside [0, duration - 1] when
 * the duration is known (duration <= 0: unknown). */
double seek_target(double from, double step, double duration);

/* Byte-size helper for the output: frames of interleaved 16-bit stereo. */
static inline double pcm_bytes_per_sec(int rate, int channels) { return (double)rate * channels * 2; }

/* Running statistics of the A/V difference (video pts shown - clock). */
typedef struct {
    long n;
    double sum, sum2, min, max;
} DiffStats;
void diff_add(DiffStats *s, double d);
double diff_mean(const DiffStats *s);
double diff_sd(const DiffStats *s);

#endif
