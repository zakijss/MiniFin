/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "proto.h"

#include <math.h>
#include <pthread.h>
#include <stdarg.h>
#include <stdlib.h>
#include <string.h>

static int parse_num(const char *s, double *out)
{
    char *end;
    double v;
    if (!s || !*s) return -1;
    v = strtod(s, &end);
    if (*end || !isfinite(v)) return -1;
    *out = v;
    return 0;
}

int args_parse(int argc, char **argv, Args *a, char *err, size_t errlen)
{
    memset(a, 0, sizeof *a);
    if (err && errlen) err[0] = 0;
    for (int i = 1; i < argc; i++) {
        const char *opt = argv[i], *val = NULL;
        char name[32];
        const char *eq;
        if (strncmp(opt, "--", 2) != 0) {
            snprintf(err, errlen, "unexpected argument %s", opt);
            return -1;
        }
        eq = strchr(opt, '=');
        if (eq) {
            size_t n = (size_t)(eq - opt);
            if (n >= sizeof name) n = sizeof name - 1;
            memcpy(name, opt, n);
            name[n] = 0;
            val = eq + 1;
        } else {
            snprintf(name, sizeof name, "%s", opt);
        }
        int known = !strcmp(name, "--url") || !strcmp(name, "--start") || !strcmp(name, "--title") ||
                    !strcmp(name, "--sub") || !strcmp(name, "--duration") || !strcmp(name, "--next") ||
                    !strcmp(name, "--prev") || !strcmp(name, "--audio") || !strcmp(name, "--audio-current") ||
                    !strcmp(name, "--subs") || !strcmp(name, "--subs-current");
        if (!known) {
            /* Skip it, and its value if it has one that is not an option. */
            if (!eq && i + 1 < argc && strncmp(argv[i + 1], "--", 2) != 0) i++;
            snprintf(err, errlen, "ignored unknown option %s", name);
            continue;
        }
        if (!val) {
            if (i + 1 >= argc) {
                snprintf(err, errlen, "%s needs a value", name);
                return -1;
            }
            val = argv[++i];
        }
        if (!strcmp(name, "--url")) a->url = val;
        else if (!strcmp(name, "--title")) a->title = val;
        else if (!strcmp(name, "--sub")) a->sub = *val ? val : NULL;
        else if (!strcmp(name, "--next")) a->next = *val ? val : NULL;
        else if (!strcmp(name, "--prev")) a->prev = *val ? val : NULL;
        else if (!strcmp(name, "--audio")) a->audio = *val ? val : NULL;
        else if (!strcmp(name, "--subs")) a->subs = *val ? val : NULL;
        else {
            double v;
            if (parse_num(val, &v) < 0) {
                snprintf(err, errlen, "%s: not a number: %s", name, val);
                return -1;
            }
            if (!strcmp(name, "--start")) a->start = v < 0 ? 0 : v;
            else if (!strcmp(name, "--audio-current")) a->audio_cur = v < 0 ? 0 : (int)v;
            else if (!strcmp(name, "--subs-current")) a->subs_cur = v < 0 ? 0 : (int)v;
            else a->duration = v < 0 ? 0 : v;
        }
    }
    if (!a->url || !*a->url) {
        snprintf(err, errlen, "--url is required");
        return -1;
    }
    return 0;
}

int track_count(const char *list)
{
    int n = 1;
    if (!list || !*list) return 0;
    for (; *list; list++) n += *list == '|';
    return n;
}

void track_name(const char *list, int i, char *buf, size_t len)
{
    buf[0] = 0;
    if (!list) return;
    while (i > 0 && *list)
        if (*list++ == '|') i--;
    if (i > 0) return;
    size_t n = strcspn(list, "|");
    if (n >= len) n = len - 1;
    memcpy(buf, list, n);
    buf[n] = 0;
}

void fmt_seconds(char *buf, size_t len, double s)
{
    if (!isfinite(s)) s = 0;
    s = floor(s * 10 + 0.5) / 10;
    if (s == 0) s = 0; /* no "-0.0" */
    snprintf(buf, len, "%.1f", s);
}

void fmt_clock(char *buf, size_t len, double s)
{
    long t = (!isfinite(s) || s < 0) ? 0 : (long)s;
    if (t >= 3600) snprintf(buf, len, "%ld:%02ld:%02ld", t / 3600, t / 60 % 60, t % 60);
    else snprintf(buf, len, "%ld:%02ld", t / 60, t % 60);
}

static FILE *out;
static pthread_mutex_t out_lock = PTHREAD_MUTEX_INITIALIZER;

void proto_set_output(FILE *f) { out = f; }

static void emit(const char *line)
{
    FILE *f = out ? out : stdout;
    pthread_mutex_lock(&out_lock);
    fputs(line, f);
    fputc('\n', f);
    fflush(f);
    pthread_mutex_unlock(&out_lock);
}

void proto_time(const char *key, double seconds)
{
    char num[32], line[64];
    fmt_seconds(num, sizeof num, seconds);
    snprintf(line, sizeof line, "%s %s", key, num);
    emit(line);
}

void proto_int(const char *key, int value)
{
    char line[64];
    snprintf(line, sizeof line, "%s %d", key, value);
    emit(line);
}

void proto_word(const char *key, const char *word)
{
    char line[128];
    snprintf(line, sizeof line, "%s %s", key, word);
    emit(line);
}

void proto_bare(const char *key) { emit(key); }

void proto_error(const char *fmt, ...)
{
    char msg[400], line[420];
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(msg, sizeof msg, fmt, ap);
    va_end(ap);
    for (char *p = msg; *p; p++)
        if ((unsigned char)*p < 0x20 || *p == 0x7f) *p = ' ';
    snprintf(line, sizeof line, "error %s", msg[0] ? msg : "unknown");
    emit(line);
}
