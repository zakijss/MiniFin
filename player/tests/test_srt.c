/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include <stdlib.h>
#include "check.h"
#include "srt.h"

static Srt parse(const char *s)
{
    Srt r;
    CHECK(srt_parse(s, strlen(s), &r) >= 0);
    return r;
}

static const char *at(const Srt *s, double t)
{
    static char buf[512];
    int hint = 0;
    srt_text_at(s, t, &hint, buf, sizeof buf);
    return buf;
}

int main(void)
{
    const char *p;
    CHECK_NEAR(srt_parse_time("00:01:02,345", &p), 62.345, 1e-9);
    CHECK_NEAR(srt_parse_time("1:02:03.5", NULL), 3723.5, 1e-9);
    CHECK_NEAR(srt_parse_time("02:03,250", NULL), 123.25, 1e-9); /* no hours */
    CHECK(srt_parse_time("00:61:00,000", NULL) < 0);
    CHECK(srt_parse_time("abc", NULL) < 0);
    CHECK(srt_parse_time("00:00:01,", NULL) < 0);

    /* basic file, CRLF, BOM, markup, tags, \N, index numbers */
    Srt s = parse("\xef\xbb\xbf" "1\r\n00:00:01,000 --> 00:00:02,500\r\nHello <i>world</i>\r\n\r\n"
                  "2\r\n00:00:03,000 --> 00:00:04,000 X1:10 X2:20\r\n{\\an8}Top line\\Nsecond line\r\n\r\n"
                  "3\r\n00:00:05,000 --> 00:00:06,000\r\n  spaced  \r\n   \r\n\r\n");
    CHECK(s.n == 3);
    CHECK_STR(at(&s, 0.5), "");
    CHECK_STR(at(&s, 1.0), "Hello world");
    CHECK_STR(at(&s, 2.49), "Hello world");
    CHECK_STR(at(&s, 2.5), "");
    CHECK_STR(at(&s, 3.5), "Top line\nsecond line");
    CHECK_STR(at(&s, 5.5), "spaced");
    srt_free(&s);

    /* unsorted, overlapping, missing blank line between cues, empty cue */
    s = parse("2\n00:00:10,000 --> 00:00:12,000\nlater\n1\n00:00:01,000 --> 00:00:20,000\nlong\n\n"
              "3\n00:00:11,000 --> 00:00:11,000\nzero length\n\n4\n00:00:30,000 --> 00:00:31,000\n<b></b>\n\n");
    CHECK(s.n == 2);
    CHECK(s.cues[0].start == 1 && s.cues[1].start == 10);
    CHECK_STR(at(&s, 11), "long\nlater");
    CHECK_STR(at(&s, 15), "long");
    srt_free(&s);

    /* Windows-1252 file (not valid UTF-8): e-acute and curly quotes */
    s = parse("1\n00:00:01,000 --> 00:00:02,000\nCaf\xe9 \x93hi\x94\n");
    CHECK(s.n == 1);
    CHECK_STR(s.cues[0].text, "Caf\xc3\xa9 \xe2\x80\x9chi\xe2\x80\x9d");
    srt_free(&s);

    /* valid UTF-8 is kept as is; a missing final newline is fine */
    s = parse("1\n00:00:01,000 --> 00:00:02,000\n\xc3\xa9t\xc3\xa9");
    CHECK(s.n == 1 && !strcmp(s.cues[0].text, "\xc3\xa9t\xc3\xa9"));
    srt_free(&s);

    /* garbage and empty input */
    s = parse("not a subtitle file\n\n--> \n");
    CHECK(s.n == 0);
    srt_free(&s);
    s = parse("");
    CHECK(s.n == 0);
    srt_free(&s);

    /* the lookup hint gives the same answers as a fresh search, forwards
     * and after jumping back */
    {
        char big[64 * 1024], *w = big;
        for (int i = 0; i < 500; i++)
            w += snprintf(w, sizeof big - (size_t)(w - big), "%d\n00:%02d:%02d,000 --> 00:%02d:%02d,500\ncue %d\n\n", i + 1, (i * 2) / 60, (i * 2) % 60,
                         (i * 2 + 1) / 60, (i * 2 + 1) % 60, i);
        s = parse(big);
        CHECK(s.n == 500);
        int hint = 0, bad = 0;
        char a[64], b[64];
        for (double t = 0; t < 1000; t += 0.37) {
            int h2 = 0;
            srt_text_at(&s, t, &hint, a, sizeof a);
            srt_text_at(&s, t, &h2, b, sizeof b);
            if (strcmp(a, b)) bad++;
        }
        srt_text_at(&s, 100.2, &hint, a, sizeof a); /* seek back */
        CHECK_STR(a, "cue 50");
        CHECK(bad == 0);
        /* a tiny buffer is cut, never overrun */
        char tiny[4];
        srt_text_at(&s, 100.2, &hint, tiny, sizeof tiny);
        CHECK_STR(tiny, "cue");
        srt_free(&s);
    }
    CHECK(srt_load("/nonexistent/file.srt", &s) < 0);
    return CHECK_DONE();
}
