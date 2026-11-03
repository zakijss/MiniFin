/* Tiny test helpers. Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_CHECK_H
#define MINIFIN_CHECK_H
#include <math.h>
#include <stdio.h>
#include <string.h>
static int check_failed;
#define CHECK(c) do { if (!(c)) { printf("%s:%d: CHECK(%s) failed\n", __FILE__, __LINE__, #c); check_failed = 1; } } while (0)
#define CHECK_STR(a, b) do { const char *a_ = (a), *b_ = (b); if (strcmp(a_, b_)) { printf("%s:%d: \"%s\" != \"%s\"\n", __FILE__, __LINE__, a_, b_); check_failed = 1; } } while (0)
#define CHECK_NEAR(a, b, eps) do { double a_ = (a), b_ = (b); if (!(fabs(a_ - b_) <= (eps))) { printf("%s:%d: %g != %g\n", __FILE__, __LINE__, a_, b_); check_failed = 1; } } while (0)
#define CHECK_DONE() (printf(check_failed ? "FAILED\n" : "ok\n"), check_failed)
#endif
