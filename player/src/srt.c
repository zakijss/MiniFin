/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "srt.h"

#include <ctype.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

/* Windows-1252 0x80..0x9f; 0 = undefined (becomes '?'). */
static const unsigned short cp1252[32] = {
    0x20ac, 0, 0x201a, 0x0192, 0x201e, 0x2026, 0x2020, 0x2021, 0x02c6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017d, 0,
    0, 0x2018, 0x2019, 0x201c, 0x201d, 0x2022, 0x2013, 0x2014, 0x02dc, 0x2122, 0x0161, 0x203a, 0x0153, 0, 0x017e, 0x0178,
};

static int valid_utf8(const unsigned char *s, size_t n)
{
    size_t i = 0;
    while (i < n) {
        unsigned c = s[i];
        int k;
        if (c < 0x80) { i++; continue; }
        if (c >= 0xc2 && c <= 0xdf) k = 1;
        else if (c >= 0xe0 && c <= 0xef) k = 2;
        else if (c >= 0xf0 && c <= 0xf4) k = 3;
        else return 0;
        for (int j = 1; j <= k; j++)
            if (i + j >= n || (s[i + j] & 0xc0) != 0x80) return 0;
        i += (size_t)k + 1;
    }
    return 1;
}

static size_t put_utf8(char *o, unsigned cp)
{
    if (cp < 0x80) { o[0] = (char)cp; return 1; }
    if (cp < 0x800) { o[0] = (char)(0xc0 | cp >> 6); o[1] = (char)(0x80 | (cp & 0x3f)); return 2; }
    o[0] = (char)(0xe0 | cp >> 12);
    o[1] = (char)(0x80 | (cp >> 6 & 0x3f));
    o[2] = (char)(0x80 | (cp & 0x3f));
    return 3;
}

/* Returns a NUL-terminated UTF-8 copy with '\r' removed. */
static char *normalise(const char *data, size_t len)
{
    const unsigned char *s = (const unsigned char *)data;
    int utf8;
    char *o, *p;
    if (len >= 3 && s[0] == 0xef && s[1] == 0xbb && s[2] == 0xbf) { s += 3; len -= 3; }
    utf8 = valid_utf8(s, len);
    o = malloc(len * 3 + 1);
    if (!o) return NULL;
    p = o;
    for (size_t i = 0; i < len; i++) {
        unsigned c = s[i];
        if (c == '\r') {
            if (i + 1 < len && s[i + 1] == '\n') continue;
            *p++ = '\n'; /* old Mac line ends */
            continue;
        }
        if (c == 0) continue;
        if (utf8 || c < 0x80) *p++ = (char)c;
        else if (c < 0xa0) p += put_utf8(p, cp1252[c - 0x80] ? cp1252[c - 0x80] : '?');
        else p += put_utf8(p, c);
    }
    *p = 0;
    return o;
}

double srt_parse_time(const char *p, const char **end)
{
    long v[3] = { 0, 0, 0 };
    int n = 0;
    double frac = 0;
    while (*p == ' ' || *p == '\t') p++;
    for (;;) {
        if (!isdigit((unsigned char)*p)) return -1;
        long x = 0;
        int digits = 0;
        while (isdigit((unsigned char)*p) && digits < 9) { x = x * 10 + (*p++ - '0'); digits++; }
        if (n >= 3) return -1;
        v[n++] = x;
        if (*p == ':') { p++; continue; }
        break;
    }
    if (n < 2) return -1;
    if (n == 2) { v[2] = v[1]; v[1] = v[0]; v[0] = 0; } /* MM:SS,mmm */
    if (*p == ',' || *p == '.') {
        double scale = 0.1;
        p++;
        if (!isdigit((unsigned char)*p)) return -1;
        while (isdigit((unsigned char)*p)) { frac += (*p++ - '0') * scale; scale /= 10; }
    }
    if (v[1] > 59 || v[2] > 59) return -1;
    if (end) *end = p;
    return v[0] * 3600.0 + v[1] * 60.0 + v[2] + frac;
}

/* Removes <i>, </font ...>, {\an8} and similar markup; "\N" becomes a
 * newline; trims each line; drops empty lines. */
static char *clean_text(const char *s, size_t n)
{
    char *o = malloc(n + 1), *p = o;
    if (!o) return NULL;
    for (size_t i = 0; i < n; i++) {
        char c = s[i];
        if (c == '<') {
            size_t j = i + 1;
            while (j < n && s[j] != '>' && s[j] != '\n') j++;
            if (j < n && s[j] == '>' && j - i <= 64) { i = j; continue; }
        }
        if (c == '{' && i + 1 < n && s[i + 1] == '\\') {
            size_t j = i + 1;
            while (j < n && s[j] != '}' && s[j] != '\n') j++;
            if (j < n && s[j] == '}') { i = j; continue; }
        }
        if (c == '\\' && i + 1 < n && (s[i + 1] == 'N' || s[i + 1] == 'n')) { *p++ = '\n'; i++; continue; }
        if (c == '\t') c = ' ';
        *p++ = c;
    }
    *p = 0;
    /* trim lines, drop empty ones */
    char *r = o, *w = o;
    while (*r) {
        char *e = strchr(r, '\n');
        size_t len = e ? (size_t)(e - r) : strlen(r);
        size_t a = 0, b = len;
        while (a < b && r[a] == ' ') a++;
        while (b > a && r[b - 1] == ' ') b--;
        if (b > a) {
            if (w != o) *w++ = '\n';
            memmove(w, r + a, b - a);
            w += b - a;
        }
        r += len + (e ? 1 : 0);
    }
    *w = 0;
    return o;
}

static int by_start(const void *a, const void *b)
{
    const SrtCue *x = a, *y = b;
    if (x->start < y->start) return -1;
    if (x->start > y->start) return 1;
    return 0;
}

static int is_timing(const char *line, double *st, double *en)
{
    const char *arrow = strstr(line, "-->");
    if (!arrow) return 0;
    *st = srt_parse_time(line, NULL);
    *en = srt_parse_time(arrow + 3, NULL);
    return *st >= 0 && *en >= 0;
}

static int is_blank(const char *line)
{
    for (; *line; line++)
        if (*line != ' ' && *line != '\t') return 0;
    return 1;
}

static int is_number(const char *line)
{
    int digits = 0;
    for (; *line; line++) {
        if (isdigit((unsigned char)*line)) digits++;
        else if (*line != ' ') return 0;
    }
    return digits > 0;
}

int srt_parse(const char *data, size_t len, Srt *out)
{
    char *buf = normalise(data, len);
    char **lines = NULL;
    int nlines = 0, cap = 0, ccap = 0;
    out->cues = NULL;
    out->n = 0;
    if (!buf) return -1;
    for (char *p = buf;;) { /* split into lines in place */
        char *nl = strchr(p, '\n');
        if (nlines == cap) {
            int nc = cap ? cap * 2 : 256;
            char **nn = realloc(lines, (size_t)nc * sizeof *nn);
            if (!nn) goto oom;
            lines = nn;
            cap = nc;
        }
        lines[nlines++] = p;
        if (!nl) break;
        *nl = 0;
        p = nl + 1;
    }
    for (int i = 0; i < nlines; i++) {
        double st, en, a, b;
        if (!is_timing(lines[i], &st, &en)) continue;
        /* text: following lines up to a blank line, a timing line, or an
         * index number directly followed by a timing line (files that
         * leave out the blank line) */
        int j = i + 1;
        size_t tl = 0;
        while (j < nlines && !is_blank(lines[j]) && !is_timing(lines[j], &a, &b) &&
               !(is_number(lines[j]) && j + 1 < nlines && is_timing(lines[j + 1], &a, &b))) {
            tl += strlen(lines[j]) + 1;
            j++;
        }
        if (en > st && j > i + 1) {
            char *joined = malloc(tl + 1), *w = joined, *txt;
            if (!joined) goto oom;
            for (int k = i + 1; k < j; k++) {
                size_t l = strlen(lines[k]);
                if (w != joined) *w++ = '\n';
                memcpy(w, lines[k], l);
                w += l;
            }
            *w = 0;
            txt = clean_text(joined, (size_t)(w - joined));
            free(joined);
            if (!txt) goto oom;
            if (!txt[0]) {
                free(txt);
            } else {
                if (out->n == ccap) {
                    int nc = ccap ? ccap * 2 : 64;
                    SrtCue *nn = realloc(out->cues, (size_t)nc * sizeof *nn);
                    if (!nn) { free(txt); goto oom; }
                    out->cues = nn;
                    ccap = nc;
                }
                out->cues[out->n].start = st;
                out->cues[out->n].end = en;
                out->cues[out->n].text = txt;
                out->n++;
            }
        }
        i = j - 1;
    }
    free(lines);
    free(buf);
    qsort(out->cues, (size_t)out->n, sizeof *out->cues, by_start);
    return out->n;
oom:
    free(lines);
    free(buf);
    srt_free(out);
    return -1;
}

int srt_load(const char *path, Srt *out)
{
    FILE *f = fopen(path, "rb");
    char *data;
    long n;
    int r;
    out->cues = NULL;
    out->n = 0;
    if (!f) return -1;
    if (fseek(f, 0, SEEK_END) != 0 || (n = ftell(f)) < 0 || n > 16 * 1024 * 1024 || fseek(f, 0, SEEK_SET) != 0) {
        fclose(f);
        return -1;
    }
    data = malloc((size_t)n + 1);
    if (!data) { fclose(f); return -1; }
    if (fread(data, 1, (size_t)n, f) != (size_t)n) { free(data); fclose(f); return -1; }
    fclose(f);
    r = srt_parse(data, (size_t)n, out);
    free(data);
    return r;
}

void srt_free(Srt *s)
{
    for (int i = 0; i < s->n; i++) free(s->cues[i].text);
    free(s->cues);
    s->cues = NULL;
    s->n = 0;
}

int srt_text_at(const Srt *s, double t, int *hint, char *buf, size_t len)
{
    int i, found = 0;
    size_t used = 0;
    if (len) buf[0] = 0;
    if (!s || !s->n) return 0;
    /* i = index of the first cue with start > t. Playback moves forward, so
     * step on from the last answer; after a seek back, binary search. */
    if (hint && *hint > 0 && *hint <= s->n && s->cues[*hint - 1].start <= t) {
        i = *hint;
        while (i < s->n && s->cues[i].start <= t) i++;
    } else {
        int lo = 0, hi = s->n;
        while (lo < hi) {
            int mid = (lo + hi) / 2;
            if (s->cues[mid].start <= t) lo = mid + 1;
            else hi = mid;
        }
        i = lo;
    }
    if (hint) *hint = i;
    /* cues are short; looking back 8 is enough for any overlap in practice */
    int first = i - 8 < 0 ? 0 : i - 8;
    for (int k = first; k < i; k++) {
        if (s->cues[k].end > t) {
            size_t l = strlen(s->cues[k].text);
            if (used && used + 1 < len) buf[used++] = '\n';
            if (used + l >= len) l = len > used + 1 ? len - used - 1 : 0;
            memcpy(buf + used, s->cues[k].text, l);
            used += l;
            if (len) buf[used] = 0;
            found++;
        }
    }
    return found;
}
