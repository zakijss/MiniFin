/* The overlay: title, position bar, messages and subtitles, drawn into an
 * upright 32-bit ARGB canvas (display.c rotates it for the panel).
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_OSD_H
#define MINIFIN_OSD_H
#include "font.h"

typedef struct {
    char title[256];
    double pos, dur;   /* seconds; dur <= 0: unknown */
    int bar;           /* show title and position bar */
    int paused;
    int volume;        /* percent */
    int subs_on, has_subs;
    char message[96];  /* short notice in the top centre; empty for none */
    char sub[512];     /* subtitle text now showing (may contain '\n') */
} OsdState;

/* Rectangle of the canvas that has visible pixels after osd_render. */
typedef struct { int x, y, w, h; } OsdRect;

#define OSD_MAX_RECTS 4

/* Clears the canvas and draws st. Fills rects (top-to-bottom, not
 * overlapping) with the areas that were drawn on; returns their count (0
 * when the overlay is empty). */
int osd_render(Canvas *c, const OsdState *st, OsdRect *rects);

#endif
