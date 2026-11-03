/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "clock.h"

#include <math.h>
#include <string.h>

void clock_reset(Clock *c) { memset(c, 0, sizeof *c); }

void clock_set(Clock *c, double pts, double now)
{
    c->pts = pts;
    c->at = now;
    c->valid = 1;
}

double clock_get(const Clock *c, double now)
{
    if (!c->valid) return NAN;
    if (c->paused) return c->pts;
    return c->pts + (now - c->at);
}

void clock_pause(Clock *c, int paused, double now)
{
    if (!!paused == !!c->paused) return;
    if (c->valid) {
        c->pts = clock_get(c, now);
        c->at = now;
    }
    c->paused = !!paused;
}

double audio_clock(double written_end, double queued_bytes, double bytes_per_sec, double fixed_latency)
{
    if (bytes_per_sec <= 0) return written_end;
    if (queued_bytes < 0) queued_bytes = 0;
    return written_end - queued_bytes / bytes_per_sec - fixed_latency;
}

int frame_action(double frame_pts, double clock, double frame_dur, int have_next, double *wait)
{
    double diff;
    if (frame_dur <= 0 || frame_dur > 0.5) frame_dur = 1.0 / 25;
    *wait = 0;
    if (isnan(clock)) { /* no clock yet: hold the frame */
        *wait = 0.005;
        return FRAME_WAIT;
    }
    diff = frame_pts - clock;
    /* A jump of many seconds is a timestamp discontinuity, not lateness:
     * show the frame and let the clock catch up. */
    if (fabs(diff) > 10) return FRAME_SHOW;
    /* Show a frame up to a quarter frame early; the pan lands about then. */
    if (diff > frame_dur / 4) {
        *wait = diff - frame_dur / 4;
        if (*wait > 0.02) *wait = 0.02; /* wake up to look at input again */
        return FRAME_WAIT;
    }
    /* More than one frame late and a newer frame is ready: skip this one. */
    if (diff < -frame_dur && have_next) return FRAME_DROP;
    return FRAME_SHOW;
}

double seek_target(double from, double step, double duration)
{
    double t = from + step;
    if (!isfinite(t)) t = 0;
    if (duration > 0 && t > duration - 1) t = duration - 1;
    if (t < 0) t = 0;
    return t;
}

void diff_add(DiffStats *s, double d)
{
    if (!s->n || d < s->min) s->min = d;
    if (!s->n || d > s->max) s->max = d;
    s->n++;
    s->sum += d;
    s->sum2 += d * d;
}

double diff_mean(const DiffStats *s) { return s->n ? s->sum / s->n : 0; }

double diff_sd(const DiffStats *s)
{
    double m, v;
    if (s->n < 2) return 0;
    m = diff_mean(s);
    v = s->sum2 / s->n - m * m;
    return v > 0 ? sqrt(v) : 0;
}
