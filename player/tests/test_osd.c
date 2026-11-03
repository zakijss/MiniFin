/* Renders the overlay states to build/host/osd-*.ppm (for a look) and
 * checks a few pixels and the reported rectangles.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include <stdlib.h>
#include "check.h"
#include "osd.h"

static void save(const Canvas *c, const char *name)
{
    char path[128];
    snprintf(path, sizeof path, "build/host/osd-%s.ppm", name);
    FILE *f = fopen(path, "wb");
    if (!f) return;
    fprintf(f, "P6\n%d %d\n255\n", c->w, c->h);
    for (int i = 0; i < c->w * c->h; i++) {
        uint32_t p = c->px[i];
        unsigned a = p >> 24;
        /* over a mid-grey "video" so transparency shows */
        unsigned char rgb[3];
        for (int k = 0; k < 3; k++) {
            unsigned v = (p >> (16 - 8 * k)) & 0xff;
            rgb[k] = (unsigned char)((v * a + 0x60 * (255 - a)) / 255);
        }
        fwrite(rgb, 1, 3, f);
    }
    fclose(f);
}

static int inside(const OsdRect *r, int n, int x, int y)
{
    for (int i = 0; i < n; i++)
        if (x >= r[i].x && x < r[i].x + r[i].w && y >= r[i].y && y < r[i].y + r[i].h) return 1;
    return 0;
}

/* every visible pixel must be inside a reported rectangle */
static int covered(const Canvas *c, const OsdRect *r, int n)
{
    for (int y = 0; y < c->h; y++)
        for (int x = 0; x < c->w; x++)
            if ((c->px[y * c->stride + x] >> 24) && !inside(r, n, x, y)) return 0;
    return 1;
}

int main(void)
{
    static uint32_t px[640 * 480];
    Canvas c = { px, 640, 480, 640 };
    OsdRect r[OSD_MAX_RECTS];
    OsdState st;
    int n;

    memset(&st, 0, sizeof st);
    n = osd_render(&c, &st, r);
    CHECK(n == 0);
    CHECK(px[0] == 0 && px[640 * 480 - 1] == 0);

    snprintf(st.title, sizeof st.title, "Big Buck Bunny (2008) - a rather long title that will not fit on one line");
    st.bar = 1;
    st.pos = 3723;
    st.dur = 5400;
    st.volume = 80;
    st.has_subs = 1;
    st.subs_on = 1;
    n = osd_render(&c, &st, r);
    CHECK(n == 2);
    CHECK(r[0].y == 0 && r[1].y + r[1].h == 480);
    CHECK((px[5 * 640 + 300] >> 24) == 0xc0); /* top bar background */
    CHECK(covered(&c, r, n));
    save(&c, "bar");

    st.paused = 1;
    snprintf(st.message, sizeof st.message, "Volume 80%%");
    snprintf(st.sub, sizeof st.sub, "This is a subtitle line long enough to wrap onto a second line here\nand a third");
    n = osd_render(&c, &st, r);
    CHECK(n == 4);
    for (int i = 1; i < n; i++) CHECK(r[i].y >= r[i - 1].y + r[i - 1].h); /* ordered, disjoint */
    CHECK(covered(&c, r, n));
    save(&c, "all");

    st.bar = 0;
    st.message[0] = 0;
    snprintf(st.sub, sizeof st.sub, "Caf\xc3\xa9 \xe2\x80\x9cquoted\xe2\x80\x9d \xe2\x80\xa6 \xe6\x97\xa5");
    n = osd_render(&c, &st, r);
    CHECK(n == 1);
    CHECK(r[0].y + r[0].h <= 480);
    CHECK(covered(&c, r, n));
    save(&c, "sub");

    st.subs_on = 0;
    n = osd_render(&c, &st, r);
    CHECK(n == 0);

    /* unknown duration: time only, no progress fill */
    st.bar = 1;
    st.dur = 0;
    n = osd_render(&c, &st, r);
    CHECK(n == 2 && covered(&c, r, n));
    CHECK(px[(480 - 64 + 44) * 640 + 20] == 0xff505050u);

    /* wrapping */
    char lines[4][160];
    int k = font_wrap("one two three four five six seven", 2, 160, &lines[0][0], 160, 4);
    CHECK(k == 4);
    CHECK_STR(lines[0], "one two");
    k = font_wrap("abcdefghijklmnopqrstuvwxyz", 2, 160, &lines[0][0], 160, 4);
    CHECK(k == 3 && !strcmp(lines[0], "abcdefghij"));
    CHECK(font_width("ab", 2) == 32);
    CHECK(font_width("\xe2\x80\xa6", 1) == 24);
    const char *s = "\xc3\xa9\xff";
    CHECK(utf8_next(&s) == 0xe9);
    CHECK(utf8_next(&s) == 0xfffd);
    CHECK(utf8_next(&s) == 0);
    return CHECK_DONE();
}
