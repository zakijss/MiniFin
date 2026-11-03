/* The screen: /dev/fb0 (640x480 BGRA, double buffered, panel mounted
 * upside down) drawn with the SigmaStar 2D engine (MI_GFX), or with the
 * CPU if that is not available.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_DISPLAY_H
#define MINIFIN_DISPLAY_H
#include <stdint.h>

#include "font.h"
#include "osd.h"

typedef struct Display Display;

/* Opens the framebuffer; hw = 1 to use MI_SYS/MI_GFX (falls back to the
 * CPU when they fail). NULL on failure. */
Display *display_open(int hw);
int display_hw(const Display *d);

/* Where the picture goes: the source's display size (after pixel aspect)
 * scaled to fit, centred. Allocates the staging surface. */
int display_set_video(Display *d, int src_w, int src_h, double sar);
void display_video_rect(const Display *d, int *x, int *y, int *w, int *h);

/* With MI_GFX the picture is a YUV 4:2:2 surface (bytes Y0 V Y1 U, the
 * order the 2D engine reads as "YUV422") that GFX converts, scales and
 * rotates onto the screen. Use one of the decoder's own surfaces ... */
void display_set_source_yuv(Display *d, uint64_t phys, int stride_bytes, int x, int y, int w, int h);
/* ... or the display's (w x h, allocated on first use; NULL on failure). */
uint8_t *display_yuv_buffer(Display *d, int w, int h, int *stride_bytes);

/* Without MI_GFX: an upright 32-bit ARGB surface the size of the video
 * rectangle that the CPU copies to the screen. */
uint32_t *display_stage(Display *d, uint64_t *phys, int *stride_px);
/* Call after the CPU wrote the YUV buffer or the stage (flushes the cache). */
void display_stage_written(Display *d);

/* MMA (physically contiguous) memory, NULL without MI_SYS. */
void *display_mma_alloc(size_t len, uint64_t *phys);
void display_mma_free(void *vir, uint64_t phys, size_t len);

/* The overlay canvas (640x480, upright). Call osd_render on it, then
 * display_osd_changed with the rectangles it returned. */
Canvas *display_osd(Display *d);
void display_osd_changed(Display *d, const OsdRect *rects, int n);

/* Composes stage + overlay into the back buffer and flips. */
void display_present(Display *d);

/* Black screen on both buffers, panned back to the first one, so MainUI
 * starts from a clean framebuffer. */
void display_blank(Display *d);
void display_close(Display *d);

/* Best effort from a signal handler: blank and pan back. */
void display_emergency_blank(void);

#endif
