/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "urlutil.h"

#include <stdio.h>
#include <string.h>
#include <math.h>
#include <stdlib.h>
#include <strings.h>
#include <ctype.h>

int url_is_network(const char *url)
{
    return !strncasecmp(url, "http://", 7) || !strncasecmp(url, "https://", 8);
}

void url_with_time(char *out, size_t len, const char *url, double t)
{
    char num[32], base[2048];
    const char *frag = strchr(url, '#');
    size_t bl = frag ? (size_t)(frag - url) : strlen(url);
    if (bl >= sizeof base) bl = sizeof base - 1;
    memcpy(base, url, bl);
    base[bl] = 0;
    if (t < 0) t = 0;
    snprintf(num, sizeof num, "%.3f", t);
    char *e = num + strlen(num) - 1; /* 742.500 -> 742.5, 10.000 -> 10 */
    while (*e == '0') *e-- = 0;
    if (*e == '.') *e = 0;
    /* drop an existing t= parameter */
    char *q = strchr(base, '?');
    if (q) {
        char *p = q + 1;
        while (*p) {
            char *amp = strchr(p, '&');
            size_t l = amp ? (size_t)(amp - p) : strlen(p);
            if (l >= 2 && p[0] == 't' && p[1] == '=') {
                if (amp) {
                    memmove(p, amp + 1, strlen(amp + 1) + 1);
                    continue;
                }
                if (p > q + 1) p[-1] = 0; /* drop the '&' before it */
                else *p = 0;
                break;
            }
            if (!amp) break;
            p = amp + 1;
        }
        if (q[1] == 0) *q = 0; /* "?" left alone */
    }
    snprintf(out, len, "%s%ct=%s%s", base, strchr(base, '?') ? '&' : '?', num, frag ? frag : "");
}

int http_header_number(const char *h, const char *name, double *out)
{
    size_t nl = strlen(name);
    const char *line = h;
    while (line && *line) {
        if (!strncasecmp(line, name, nl) && line[nl] == ':') {
            const char *v = line + nl + 1;
            char *end;
            while (*v == ' ' || *v == '\t') v++;
            double x = strtod(v, &end);
            if (end == v || !isfinite(x)) return 0;
            while (*end == ' ' || *end == '\t') end++;
            if (*end && *end != '\r' && *end != '\n') return 0;
            *out = x;
            return 1;
        }
        line = strchr(line, '\n');
        if (line) line++;
    }
    return 0;
}

int url_split_http(const char *url, char *host, size_t hostlen, int *port, char *path, size_t pathlen)
{
    const char *p, *slash, *colon;
    size_t hl;
    if (strncasecmp(url, "http://", 7)) return -1;
    p = url + 7;
    slash = strchr(p, '/');
    if (!slash) slash = p + strlen(p);
    colon = memchr(p, ':', (size_t)(slash - p));
    hl = (size_t)((colon ? colon : slash) - p);
    if (!hl || hl >= hostlen) return -1;
    memcpy(host, p, hl);
    host[hl] = 0;
    *port = colon ? atoi(colon + 1) : 80;
    if (*port <= 0 || *port > 65535) return -1;
    snprintf(path, pathlen, "%s", *slash ? slash : "/");
    return 0;
}
