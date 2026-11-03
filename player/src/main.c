/* minifin-player: plays one MPEG-TS (H.264 + AAC) stream or file on the
 * Miyoo Mini Plus with the SoC's video decoder, for MiniFin.
 *
 *   minifin-player --url URL|PATH [--start S] [--title T] [--sub FILE.srt] [--duration D]
 *
 * stdout: dur, decoder, pos, paused, end, quit, error lines (see README).
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include "log.h"
#include "player.h"
#include "proto.h"

static void on_stop(int sig) { player_request_quit(); }

static void on_crash(int sig)
{
    player_emergency_cleanup();
    signal(sig, SIG_DFL);
    raise(sig);
}

/* Shutdown took too long (a stuck read or driver call): release what
 * matters and leave. The "quit"/"end"/"error" line was already printed. */
static void on_alarm(int sig)
{
    static const char msg[] = "minifin-player: shutdown timed out\n";
    player_emergency_cleanup();
    if (write(2, msg, sizeof msg - 1) < 0) {}
    _exit(0);
}

int main(int argc, char **argv)
{
    Args a;
    char err[200];
    struct sigaction sa;
    setvbuf(stdout, NULL, _IOLBF, 0);
    if (args_parse(argc, argv, &a, err, sizeof err) < 0) {
        proto_error("%s", err);
        fprintf(stderr, "usage: minifin-player --url URL|PATH [--start S] [--title T] [--sub FILE.srt] [--duration D]\n");
        return 2;
    }
    if (err[0]) plog("%s", err);
    memset(&sa, 0, sizeof sa);
    sa.sa_handler = on_stop;
    sigaction(SIGTERM, &sa, NULL);
    sigaction(SIGINT, &sa, NULL);
    sigaction(SIGHUP, &sa, NULL);
    sigaction(SIGQUIT, &sa, NULL);
    sa.sa_handler = on_alarm;
    sigaction(SIGALRM, &sa, NULL);
    signal(SIGPIPE, SIG_IGN);
    sa.sa_handler = on_crash;
    sa.sa_flags = SA_RESETHAND;
    sigaction(SIGSEGV, &sa, NULL);
    sigaction(SIGBUS, &sa, NULL);
    sigaction(SIGILL, &sa, NULL);
    sigaction(SIGFPE, &sa, NULL);
    sigaction(SIGABRT, &sa, NULL);
    plog("url %s start %.1f title \"%s\" sub %s", a.url, a.start, a.title ? a.title : "", a.sub ? a.sub : "-");
    return player_run(&a);
}
