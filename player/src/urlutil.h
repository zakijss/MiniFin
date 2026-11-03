/* URL helpers. Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_URLUTIL_H
#define MINIFIN_URLUTIL_H
#include <stddef.h>

/* "http://h/v.ts" + 742.5 -> "http://h/v.ts?t=742.5"; "&t=" when the URL
 * already has a query. An existing t= parameter is replaced; a #fragment
 * stays at the end. */
void url_with_time(char *out, size_t len, const char *url, double t);

/* 1 for http:// and https:// (case-insensitive); everything else is a file. */
int url_is_network(const char *url);

/* Finds "Name: value" in a block of HTTP response headers (name matched
 * case-insensitively at the start of a line) and parses value as a number.
 * 1 found, 0 not. */
int http_header_number(const char *headers, const char *name, double *out);

/* Splits http://host[:port]/path into its parts. 0 ok, -1 if not http://. */
int url_split_http(const char *url, char *host, size_t hostlen, int *port, char *path, size_t pathlen);

#endif
