/* SubRip (.srt) subtitles.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_SRT_H
#define MINIFIN_SRT_H
#include <stddef.h>

typedef struct {
    double start, end; /* seconds */
    char *text;        /* UTF-8, lines separated by '\n', markup removed */
} SrtCue;

typedef struct {
    SrtCue *cues; /* sorted by start */
    int n;
} Srt;

/* Parses a whole file's bytes. UTF-8 (with or without BOM); a file that is
 * not valid UTF-8 is read as Windows-1252. Returns the number of cues, or
 * -1 on allocation failure. Malformed blocks are skipped. */
int srt_parse(const char *data, size_t len, Srt *out);
int srt_load(const char *path, Srt *out); /* -1 if unreadable */
void srt_free(Srt *s);

/* Text of every cue showing at time t joined with '\n' into buf (empty when
 * none). *hint speeds up lookups for increasing t; start it at 0. Returns
 * the number of cues found. */
int srt_text_at(const Srt *s, double t, int *hint, char *buf, size_t len);

/* "00:01:02,345" -> 62.345; -1 if malformed. Exposed for tests. */
double srt_parse_time(const char *p, const char **end);

#endif
