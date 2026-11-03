/* Sound out through Onion's audioserver, the way Onion's apps do it: OSS
 * /dev/dsp calls handled by /customer/lib/libpadsp.so, which forwards the
 * samples to audioserver over a FIFO. The library is dlopen()ed, so the
 * player needs no LD_PRELOAD.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_AUDIO_H
#define MINIFIN_AUDIO_H
#include <stdint.h>

typedef struct AOut AOut;

/* Interleaved signed 16-bit stereo at about 'rate'. NULL when there is no
 * way to play sound. */
AOut *aout_open(int rate);
int aout_rate(const AOut *a);
/* Blocks while the queue is full. Returns 0 or -1. */
int aout_write(AOut *a, const int16_t *pcm, int frames);
/* Seconds of sound written but not yet heard. */
double aout_queued(AOut *a);
void aout_close(AOut *a);

#endif
