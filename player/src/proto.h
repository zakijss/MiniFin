/* Command line and the stdout protocol MiniFin reads.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_PROTO_H
#define MINIFIN_PROTO_H
#include <stddef.h>
#include <stdio.h>

typedef struct {
    const char *url;   /* --url: http(s):// URL or a local file path */
    double start;      /* --start: video time at which the input begins */
    const char *title; /* --title: shown in the overlay */
    const char *sub;   /* --sub: SubRip file */
    double duration;   /* --duration (optional): length of the video, for the bar
                        * when the stream does not know it */
    const char *next;  /* --next: title of the next episode (R2), NULL: none */
    const char *prev;  /* --prev: title of the previous episode (L2) */
    const char *audio; /* --audio: audio track names, "|" between them (X) */
    int audio_cur;     /* --audio-current: the one playing, from 0 */
    const char *subs;  /* --subs: subtitle choices, "|" between them (SELECT) */
    int subs_cur;      /* --subs-current */
} Args;

/* Parses argv. Returns 0, or -1 with a message in err. Unknown options
 * are skipped (with a note in err, return 0) so a newer app does not make
 * the player fail. */
int args_parse(int argc, char **argv, Args *a, char *err, size_t errlen);

/* Tracks in a "|" list: how many, and the name of number i (from 0). */
int track_count(const char *list);
void track_name(const char *list, int i, char *buf, size_t len);

/* "%.1f" without a negative zero, never "nan". */
void fmt_seconds(char *buf, size_t len, double s);

/* "1:02:03" or "2:03" (negative and unknown times print "0:00"). */
void fmt_clock(char *buf, size_t len, double s);

/* One protocol line, written and flushed atomically (thread-safe).
 * proto_line(out, "pos", 12.3) -> "pos 12.3\n". */
void proto_set_output(FILE *f);
void proto_time(const char *key, double seconds);
void proto_int(const char *key, int value);
void proto_word(const char *key, const char *word);
void proto_bare(const char *key);
/* "error <message>": newlines and control characters become spaces. */
void proto_error(const char *fmt, ...) __attribute__((format(printf, 1, 2)));

#endif
