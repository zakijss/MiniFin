/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "check.h"
#include "clock.h"

int main(void)
{
    Clock c;
    double w;
    clock_reset(&c);
    CHECK(isnan(clock_get(&c, 1)));
    clock_set(&c, 10, 100);
    CHECK_NEAR(clock_get(&c, 100.5), 10.5, 1e-9);
    clock_pause(&c, 1, 101);
    CHECK_NEAR(clock_get(&c, 200), 11, 1e-9); /* stands still while paused */
    clock_pause(&c, 0, 200);
    CHECK_NEAR(clock_get(&c, 201), 12, 1e-9);
    clock_pause(&c, 0, 300); /* no-op */
    CHECK_NEAR(clock_get(&c, 202), 13, 1e-9);

    /* audio clock: 48 kHz stereo s16 = 192000 B/s; 19200 B queued = 0.1 s */
    CHECK_NEAR(audio_clock(5.0, 19200, pcm_bytes_per_sec(48000, 2), 0.05), 4.85, 1e-9);
    CHECK_NEAR(audio_clock(5.0, -1, 192000, 0), 5.0, 1e-9);

    /* frame decisions at 30 fps */
    double fd = 1.0 / 30;
    CHECK(frame_action(1.0, NAN, fd, 0, &w) == FRAME_WAIT && w > 0);
    CHECK(frame_action(1.0, 0.9, fd, 0, &w) == FRAME_WAIT && w <= 0.02 && w > 0);
    CHECK(frame_action(1.0, 0.995, fd, 0, &w) == FRAME_SHOW); /* within a quarter frame */
    CHECK(frame_action(1.0, 1.02, fd, 1, &w) == FRAME_SHOW);  /* a little late: show */
    CHECK(frame_action(1.0, 1.05, fd, 1, &w) == FRAME_DROP);  /* > 1 frame late, newer ready */
    CHECK(frame_action(1.0, 1.05, fd, 0, &w) == FRAME_SHOW);  /* nothing newer: show */
    CHECK(frame_action(100.0, 1.0, fd, 0, &w) == FRAME_SHOW); /* discontinuity */
    CHECK(frame_action(1.0, 0.995, 0, 0, &w) == FRAME_SHOW);   /* bad frame duration -> 25 fps */

    /* seek targets */
    CHECK_NEAR(seek_target(5, -10, 100), 0, 0);
    CHECK_NEAR(seek_target(95, 10, 100), 99, 0);
    CHECK_NEAR(seek_target(95, 10, 0), 105, 0); /* unknown duration */
    CHECK_NEAR(seek_target(30, -10, 100), 20, 0);

    /* drift statistics */
    DiffStats s = { 0 };
    diff_add(&s, 0.01);
    diff_add(&s, -0.01);
    diff_add(&s, 0.03);
    CHECK_NEAR(diff_mean(&s), 0.01, 1e-12);
    CHECK(s.min == -0.01 && s.max == 0.03);
    CHECK_NEAR(diff_sd(&s), 0.0163299, 1e-6);

    /* simulate 10 minutes of playback: the audio device plays 0.05 % fast;
     * video frames scheduled against the audio clock never drift from it */
    {
        double rate = 48000 * 1.0005, written = 0, t, worst = 0;
        int frame = 0;
        for (t = 0; t < 600; t += 0.001) {
            double played = t * rate / 48000; /* media seconds the speaker played */
            while (written < played + 0.2) written += 1024.0 / 48000; /* keep 0.2 s queued */
            double queued = (written - played) * 192000;
            double clk = audio_clock(written, queued, 192000, 0);
            double pts = frame / 30.0;
            if (frame_action(pts, clk, fd, 0, &w) != FRAME_WAIT) {
                double err = pts - played;
                if (fabs(err) > worst) worst = fabs(err);
                frame++;
            }
        }
        CHECK(worst < fd);
        CHECK(frame > 30 * 599);
    }
    return CHECK_DONE();
}
