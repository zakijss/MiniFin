/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "font.h"

#include <string.h>

#include "third_party/font8x8.h"

unsigned utf8_next(const char **sp)
{
    const unsigned char *s = (const unsigned char *)*sp;
    unsigned c = s[0], cp;
    int k;
    if (!c) return 0;
    if (c < 0x80) { *sp += 1; return c; }
    if (c >= 0xc2 && c <= 0xdf) { k = 1; cp = c & 0x1f; }
    else if (c >= 0xe0 && c <= 0xef) { k = 2; cp = c & 0x0f; }
    else if (c >= 0xf0 && c <= 0xf4) { k = 3; cp = c & 0x07; }
    else { *sp += 1; return 0xfffd; }
    for (int i = 1; i <= k; i++) {
        if ((s[i] & 0xc0) != 0x80) { *sp += i; return 0xfffd; }
        cp = cp << 6 | (s[i] & 0x3f);
    }
    *sp += k + 1;
    return cp;
}

/* Typographic punctuation that subtitles use a lot, folded to ASCII. */
static unsigned fold(unsigned cp)
{
    switch (cp) {
    case 0x2018: case 0x2019: case 0x201a: case 0x2032: return '\'';
    case 0x201c: case 0x201d: case 0x201e: case 0x2033: return '"';
    case 0x2010: case 0x2011: case 0x2012: case 0x2013: case 0x2014: case 0x2015: case 0x2212: return '-';
    case 0x2026: return 0x2026; /* drawn as "..." by the caller */
    case 0x00a0: case 0x2009: case 0x200a: case 0x202f: return ' ';
    case 0x266a: case 0x266b: return '#';
    case 0x20ac: return 'E';
    default: return cp;
    }
}

static const unsigned char *glyph(unsigned cp)
{
    static const unsigned char question[8] = { 0x1E, 0x33, 0x30, 0x18, 0x0C, 0x00, 0x0C, 0x00 };
    cp = fold(cp);
    if (cp < 0x80) return (const unsigned char *)font8x8_basic[cp];
    if (cp >= 0xa0 && cp <= 0xff) return (const unsigned char *)font8x8_ext_latin[cp - 0xa0];
    return question;
}

int font_width(const char *s, int scale)
{
    int n = 0;
    unsigned cp;
    while ((cp = utf8_next(&s)) != 0) {
        if (cp == '\n') break;
        n += fold(cp) == 0x2026 ? 3 : 1;
    }
    return n * 8 * scale;
}

static void put_glyph(Canvas *c, int x, int y, const unsigned char *g, int scale, uint32_t color)
{
    for (int row = 0; row < 8; row++) {
        unsigned bits = g[row];
        if (!bits) continue;
        for (int col = 0; col < 8; col++) {
            if (!(bits >> col & 1)) continue; /* LSB is the leftmost pixel */
            int x0 = x + col * scale, y0 = y + row * scale;
            for (int dy = 0; dy < scale; dy++) {
                int yy = y0 + dy;
                if (yy < 0 || yy >= c->h) continue;
                uint32_t *line = c->px + (size_t)yy * c->stride;
                for (int dx = 0; dx < scale; dx++) {
                    int xx = x0 + dx;
                    if (xx >= 0 && xx < c->w) line[xx] = color;
                }
            }
        }
    }
}

static void draw_run(Canvas *c, int x, int y, const char *s, int scale, uint32_t color)
{
    unsigned cp;
    while ((cp = utf8_next(&s)) != 0 && cp != '\n') {
        if (fold(cp) == 0x2026) {
            for (int i = 0; i < 3; i++, x += 8 * scale) put_glyph(c, x, y, glyph('.'), scale, color);
            continue;
        }
        put_glyph(c, x, y, glyph(cp), scale, color);
        x += 8 * scale;
    }
}

void font_draw(Canvas *c, int x, int y, const char *s, int scale, uint32_t color, uint32_t outline)
{
    if (outline >> 24) {
        int o = scale > 1 ? scale / 2 + 1 : 1;
        for (int dy = -o; dy <= o; dy += o)
            for (int dx = -o; dx <= o; dx += o)
                if (dx || dy) draw_run(c, x + dx, y + dy, s, scale, outline);
    }
    draw_run(c, x, y, s, scale, color);
}

int font_wrap(const char *s, int scale, int maxw, char *out, int linelen, int maxlines)
{
    int n = 0, cw = 8 * scale, maxc = maxw / cw;
    if (maxc < 1) maxc = 1;
    while (*s && n < maxlines) {
        /* one source line */
        const char *e = strchr(s, '\n');
        size_t sl = e ? (size_t)(e - s) : strlen(s);
        const char *p = s, *end = s + sl;
        while (p < end && n < maxlines) {
            /* take as many code points as fit, then back up to a space */
            const char *q = p, *last_space = NULL, *fit_end = p;
            int cols = 0;
            while (q < end) {
                const char *before = q;
                unsigned cp = utf8_next(&q);
                int w = fold(cp) == 0x2026 ? 3 : 1;
                if (cols + w > maxc) { q = before; break; }
                cols += w;
                if (cp == ' ') last_space = before;
                fit_end = q;
            }
            const char *cut = fit_end, *next = fit_end;
            if (fit_end < end && last_space && last_space > p) { cut = last_space; next = last_space + 1; }
            size_t l = (size_t)(cut - p);
            if ((int)l >= linelen) l = (size_t)linelen - 1;
            char *line = out + (size_t)n * (size_t)linelen;
            memcpy(line, p, l);
            line[l] = 0;
            /* drop trailing spaces */
            while (l && line[l - 1] == ' ') line[--l] = 0;
            n++;
            p = next;
            while (p < end && *p == ' ') p++;
        }
        s = e ? e + 1 : end;
    }
    return n;
}

void canvas_fill(Canvas *c, int x, int y, int w, int h, uint32_t color)
{
    if (x < 0) { w += x; x = 0; }
    if (y < 0) { h += y; y = 0; }
    if (x + w > c->w) w = c->w - x;
    if (y + h > c->h) h = c->h - y;
    for (int yy = y; yy < y + h; yy++) {
        uint32_t *line = c->px + (size_t)yy * c->stride + x;
        for (int i = 0; i < w; i++) line[i] = color;
    }
}
