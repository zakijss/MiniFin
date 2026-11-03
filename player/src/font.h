/* 8x8 bitmap text (font8x8, public domain) drawn into 32-bit ARGB buffers.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_FONT_H
#define MINIFIN_FONT_H
#include <stddef.h>
#include <stdint.h>

typedef struct {
    uint32_t *px; /* 0xAARRGGBB, row-major */
    int w, h, stride; /* stride in pixels */
} Canvas;

/* Next code point of UTF-8 text (invalid bytes come back as U+FFFD). */
unsigned utf8_next(const char **s);

/* Width in pixels of text drawn at scale (each glyph 8*scale wide). */
int font_width(const char *utf8, int scale);

/* Draws text with its top-left at x,y. outline != 0 draws a 1*scale/2 px
 * border in that colour first (alpha 0 = no border). */
void font_draw(Canvas *c, int x, int y, const char *utf8, int scale, uint32_t color, uint32_t outline);

/* Breaks text into at most maxlines lines no wider than maxw pixels,
 * breaking at spaces (a word longer than a line is cut). Existing '\n'
 * are kept. Lines are written to out (NUL-separated, each <= linelen).
 * Returns the number of lines. */
int font_wrap(const char *utf8, int scale, int maxw, char *out, int linelen, int maxlines);

void canvas_fill(Canvas *c, int x, int y, int w, int h, uint32_t color);

#endif
