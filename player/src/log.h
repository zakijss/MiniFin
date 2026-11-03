/* Diagnostics go to stderr (stdout is the protocol). MINIFIN_QUIET=1 hides them.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_LOG_H
#define MINIFIN_LOG_H
void plog(const char *fmt, ...) __attribute__((format(printf, 1, 2)));
double now_sec(void); /* CLOCK_MONOTONIC */
#endif
