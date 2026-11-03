/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "log.h"

#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <time.h>

double now_sec(void)
{
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return ts.tv_sec + ts.tv_nsec / 1e9;
}

void plog(const char *fmt, ...)
{
    static double t0;
    static int quiet = -1;
    char buf[512];
    va_list ap;
    if (quiet < 0) {
        const char *q = getenv("MINIFIN_QUIET");
        quiet = q && *q == '1';
    }
    if (quiet) return;
    if (t0 == 0) t0 = now_sec();
    va_start(ap, fmt);
    vsnprintf(buf, sizeof buf, fmt, ap);
    va_end(ap);
    fprintf(stderr, "minifin-player %8.3f %s\n", now_sec() - t0, buf);
}
