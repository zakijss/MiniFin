/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include <stdlib.h>
#include "check.h"
#include "proto.h"


int main(void)
{
    Args a;
    char err[128], buf[32];
    {
        char *v[] = { "p", "--url", "http://127.0.0.1:8080/x.ts", "--start", "12.5", "--title", "A \"film\"", "--sub", "/tmp/a.srt" };
        CHECK(args_parse(9, v, &a, err, sizeof err) == 0);
        CHECK_STR(a.url, "http://127.0.0.1:8080/x.ts");
        CHECK_NEAR(a.start, 12.5, 0);
        CHECK_STR(a.title, "A \"film\"");
        CHECK_STR(a.sub, "/tmp/a.srt");
        CHECK(a.duration == 0);
    }
    {
        char *v[] = { "p", "--url=/mnt/SDCARD/x.ts", "--start=7", "--offset", "600", "--duration=5400" };
        CHECK(args_parse(6, v, &a, err, sizeof err) == 0);
        CHECK_STR(a.url, "/mnt/SDCARD/x.ts");
        CHECK(a.start == 7 && a.duration == 5400); /* --offset is unknown: skipped with its value */
        CHECK(strstr(err, "--offset") != NULL);
        CHECK(a.title == NULL && a.sub == NULL);
    }
    {
        char *v[] = { "p", "--start", "5" };
        CHECK(args_parse(3, v, &a, err, sizeof err) < 0); /* no url */
        char *w[] = { "p", "--url", "u", "--start", "abc" };
        CHECK(args_parse(5, w, &a, err, sizeof err) < 0);
        char *x[] = { "p", "--url" };
        CHECK(args_parse(2, x, &a, err, sizeof err) < 0);
        char *y[] = { "p", "--url", "u", "--start", "-3" };
        CHECK(args_parse(5, y, &a, err, sizeof err) == 0 && a.start == 0);
        char *z[] = { "p", "--url", "u", "--future", "1", "--flag", "--sub", "" };
        CHECK(args_parse(8, z, &a, err, sizeof err) == 0 && a.sub == NULL);
        CHECK(strstr(err, "ignored") != NULL);
        char *q[] = { "p", "stray" };
        CHECK(args_parse(2, q, &a, err, sizeof err) < 0);
    }
    {
        char *v[] = { "p", "--url", "u", "--next", "S1E3 Rain", "--prev", "", "--audio", "English|Japanese|Commentary",
                      "--audio-current", "1", "--subs=Off|English", "--subs-current=1" };
        CHECK(args_parse(13, v, &a, err, sizeof err) == 0);
        CHECK_STR(a.next, "S1E3 Rain");
        CHECK(a.prev == NULL && a.audio_cur == 1 && a.subs_cur == 1);
        CHECK(track_count(a.audio) == 3 && track_count(a.subs) == 2 && track_count(NULL) == 0 && track_count("") == 0);
        track_name(a.audio, 0, buf, sizeof buf); CHECK_STR(buf, "English");
        track_name(a.audio, 2, buf, sizeof buf); CHECK_STR(buf, "Commentary");
        track_name(a.audio, 3, buf, sizeof buf); CHECK_STR(buf, "");
        track_name("A very long track name that does not fit", 0, buf, 8); CHECK_STR(buf, "A very ");
    }
    fmt_seconds(buf, sizeof buf, 12.34); CHECK_STR(buf, "12.3");
    fmt_seconds(buf, sizeof buf, 12.36); CHECK_STR(buf, "12.4");
    fmt_seconds(buf, sizeof buf, -0.01); CHECK_STR(buf, "0.0");
    fmt_seconds(buf, sizeof buf, 0.0 / 0.0); CHECK_STR(buf, "0.0");
    fmt_seconds(buf, sizeof buf, 5400); CHECK_STR(buf, "5400.0");
    fmt_clock(buf, sizeof buf, 0); CHECK_STR(buf, "0:00");
    fmt_clock(buf, sizeof buf, 75.9); CHECK_STR(buf, "1:15");
    fmt_clock(buf, sizeof buf, 3723); CHECK_STR(buf, "1:02:03");
    fmt_clock(buf, sizeof buf, -5); CHECK_STR(buf, "0:00");

    /* the lines MiniFin reads */
    {
        char *out = NULL;
        size_t len = 0;
        FILE *f = open_memstream(&out, &len);
        proto_set_output(f);
        proto_time("dur", 5400.04);
        proto_word("decoder", "hw");
        proto_time("pos", 1.24);
        proto_int("paused", 1);
        proto_bare("end");
        proto_time("quit", 61.0);
        proto_error("bad\nthing %d", 3);
        fclose(f);
        CHECK_STR(out, "dur 5400.0\ndecoder hw\npos 1.2\npaused 1\nend\nquit 61.0\nerror bad thing 3\n");
        free(out);
        proto_set_output(NULL);
    }
    return CHECK_DONE();
}

