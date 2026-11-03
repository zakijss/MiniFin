/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "osd.h"

#include <stdio.h>
#include <string.h>

#include "proto.h"

#define BAR_BG   0xc0000000u /* black, 75 % */
#define WHITE    0xffffffffu
#define GREY     0xffa0a0a0u
#define TRACK    0xff505050u
#define ACCENT   0xff3fa9f5u
#define OUTLINE  0xff000000u

#define TOP_H 40
#define BOT_H 64

static void add_rect(OsdRect *r, int *n, int x, int y, int w, int h)
{
    if (*n < OSD_MAX_RECTS && w > 0 && h > 0) {
        r[*n].x = x;
        r[*n].y = y;
        r[*n].w = w;
        r[*n].h = h;
        (*n)++;
    }
}

/* Cuts text to fit maxw at scale, ending in "..." when cut. */
static void fit_text(char *dst, size_t len, const char *src, int scale, int maxw)
{
    snprintf(dst, len, "%s", src);
    if (font_width(dst, scale) <= maxw) return;
    size_t n = strlen(dst);
    while (n > 0) {
        n--;
        while (n > 0 && ((unsigned char)dst[n] & 0xc0) == 0x80) n--; /* whole code points */
        dst[n] = 0;
        char tmp[300];
        snprintf(tmp, sizeof tmp, "%s...", dst);
        if (font_width(tmp, scale) <= maxw) {
            snprintf(dst, len, "%s", tmp);
            return;
        }
    }
}

static void pause_icon(Canvas *c, int x, int y, int paused)
{
    if (paused) { /* two bars */
        canvas_fill(c, x, y, 6, 20, WHITE);
        canvas_fill(c, x + 11, y, 6, 20, WHITE);
    } else { /* play triangle */
        for (int i = 0; i < 20; i++) {
            int w = i < 10 ? i + 1 : 20 - i;
            canvas_fill(c, x, y + i, w * 17 / 10, 1, WHITE);
        }
    }
}

int osd_render(Canvas *c, const OsdState *st, OsdRect *rects)
{
    int n = 0;
    int sub_bottom = c->h - 12;
    memset(c->px, 0, (size_t)c->stride * (size_t)c->h * 4);

    if (st->bar) {
        char t[300], a[32], b[32], line[80];
        /* top: title */
        canvas_fill(c, 0, 0, c->w, TOP_H, BAR_BG);
        fit_text(t, sizeof t, st->title[0] ? st->title : "MiniFin", 2, c->w - 24);
        font_draw(c, 12, (TOP_H - 16) / 2, t, 2, WHITE, 0);
        add_rect(rects, &n, 0, 0, c->w, TOP_H);

        /* bottom: play state, time, progress, volume and subtitle state */
        int y0 = c->h - BOT_H;
        canvas_fill(c, 0, y0, c->w, BOT_H, BAR_BG);
        pause_icon(c, 14, y0 + 10, st->paused);
        fmt_clock(a, sizeof a, st->pos);
        if (st->dur > 0) {
            fmt_clock(b, sizeof b, st->dur);
            snprintf(line, sizeof line, "%s / %s", a, b);
        } else {
            snprintf(line, sizeof line, "%s", a);
        }
        font_draw(c, 44, y0 + 12, line, 2, WHITE, 0);
        snprintf(line, sizeof line, "Vol %d%%%s", st->volume, st->has_subs ? (st->subs_on ? "  Subs on" : "  Subs off") : "");
        int lw = font_width(line, 1) * 2;
        font_draw(c, c->w - 14 - lw, y0 + 12, line, 2, GREY, 0);
        /* progress track */
        int tx = 14, tw = c->w - 28, ty = y0 + 44;
        canvas_fill(c, tx, ty, tw, 6, TRACK);
        if (st->dur > 0) {
            double f = st->pos / st->dur;
            if (f < 0) f = 0;
            if (f > 1) f = 1;
            int fw = (int)(f * tw + 0.5);
            canvas_fill(c, tx, ty, fw, 6, ACCENT);
            canvas_fill(c, tx + fw - 3, ty - 4, 6, 14, WHITE);
        }
        add_rect(rects, &n, 0, y0, c->w, BOT_H);
        sub_bottom = y0 - 8;
    }

    if (st->message[0]) {
        char m[120];
        fit_text(m, sizeof m, st->message, 2, c->w - 48);
        int w = font_width(m, 2) + 32, h = 36;
        int x = (c->w - w) / 2, y = st->bar ? TOP_H + 16 : 24;
        canvas_fill(c, x, y, w, h, BAR_BG);
        font_draw(c, x + 16, y + 10, m, 2, WHITE, 0);
        /* keep rects ordered top to bottom */
        if (n == 2) {
            OsdRect bottom = rects[1];
            n = 1;
            add_rect(rects, &n, x, y, w, h);
            rects[n++] = bottom;
        } else {
            add_rect(rects, &n, x, y, w, h);
        }
    }

    if (st->subs_on && st->sub[0]) {
        char lines[4][160];
        int scale = 2, lh = 8 * scale + 6;
        int k = font_wrap(st->sub, scale, c->w - 32, &lines[0][0], 160, 4);
        int top = sub_bottom - k * lh, minx = c->w, maxx = 0;
        for (int i = 0; i < k; i++) {
            int w = font_width(lines[i], scale);
            int x = (c->w - w) / 2;
            font_draw(c, x, top + i * lh, lines[i], scale, WHITE, OUTLINE);
            if (x < minx) minx = x;
            if (x + w > maxx) maxx = x + w;
        }
        if (k) {
            int pad = scale + 2;
            OsdRect r = { minx - pad, top - pad, maxx - minx + 2 * pad, k * lh + 2 * pad };
            if (r.x < 0) { r.w += r.x; r.x = 0; }
            if (r.x + r.w > c->w) r.w = c->w - r.x;
            /* subtitles sit above the bottom bar: insert before it */
            if (st->bar && n >= 2) {
                OsdRect bottom = rects[n - 1];
                n--;
                add_rect(rects, &n, r.x, r.y, r.w, r.h);
                rects[n++] = bottom;
            } else {
                add_rect(rects, &n, r.x, r.y, r.w, r.h);
            }
        }
    }
    return n;
}
