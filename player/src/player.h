/* The player: runs one video until the end, a stop, or an error.
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_PLAYER_H
#define MINIFIN_PLAYER_H
#include "proto.h"

/* Returns the process exit code: 0 after "end" or "quit", 1 after "error". */
int player_run(const Args *args);

/* For signal handlers: ask the player to stop (prints "quit <pos>"). */
void player_request_quit(void);
/* Best-effort release of the decoder and screen (crash handler). */
void player_emergency_cleanup(void);

#endif
