/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "audio.h"

#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/soundcard.h>
#include <sys/syscall.h>
#include <unistd.h>

#include "log.h"

struct AOut {
    void *lib;
    int (*p_open)(const char *, int, ...);
    int (*p_ioctl)(int, unsigned long, ...);
    ssize_t (*p_write)(int, const void *, size_t);
    int (*p_close)(int);
    int fd;
    int rate;
    int fionread;     /* the fd is a pipe we can measure */
    double latency;   /* audioserver + DMA, seconds (estimate) */
    /* fallback estimate when the queue cannot be measured */
    double written, t0;
};

static const char *libs[] = { "libpadsp.so", "/customer/lib/libpadsp.so", "/mnt/SDCARD/miyoo/lib/libpadsp.so" };

AOut *aout_open(int rate)
{
    AOut *a = calloc(1, sizeof *a);
    const char *e;
    int v;
    if (!a) return NULL;
    for (unsigned i = 0; i < sizeof libs / sizeof *libs && !a->lib; i++) a->lib = dlopen(libs[i], RTLD_NOW | RTLD_LOCAL);
    if (!a->lib) {
        plog("audio: no libpadsp.so (%s)", dlerror());
        free(a);
        return NULL;
    }
    a->p_open = (int (*)(const char *, int, ...))dlsym(a->lib, "open");
    a->p_ioctl = (int (*)(int, unsigned long, ...))dlsym(a->lib, "ioctl");
    a->p_write = (ssize_t(*)(int, const void *, size_t))dlsym(a->lib, "write");
    a->p_close = (int (*)(int))dlsym(a->lib, "close");
    if (!a->p_open || !a->p_ioctl || !a->p_write || !a->p_close) {
        plog("audio: libpadsp.so lacks open/ioctl/write/close");
        dlclose(a->lib);
        free(a);
        return NULL;
    }
    /* libpadsp looks up the real open() on its first call for an ordinary
     * path, but its /dev/dsp path calls through that pointer unchecked: as
     * the first call (it is dlopen()ed here, not preloaded) it jumps to NULL.
     * An ordinary open first fills the pointer in. */
    v = a->p_open("/dev/null", O_RDONLY);
    if (v >= 0) a->p_close(v);
    /* libpadsp prints notes ("new pipe size: ...") on stdout, which is our
     * protocol: point stdout at stderr while it opens. */
    fflush(stdout);
    int saved = dup(1);
    if (saved >= 0) dup2(2, 1);
    a->fd = a->p_open("/dev/dsp", O_WRONLY);
    fflush(stdout);
    if (saved >= 0) {
        dup2(saved, 1);
        close(saved);
    }
    if (a->fd < 0) {
        plog("audio: open /dev/dsp via padsp: %s", strerror(errno));
        dlclose(a->lib);
        free(a);
        return NULL;
    }
    v = AFMT_S16_LE;
    a->p_ioctl(a->fd, SNDCTL_DSP_SETFMT, &v);
    v = 2;
    a->p_ioctl(a->fd, SNDCTL_DSP_CHANNELS, &v);
    v = rate;
    if (a->p_ioctl(a->fd, SNDCTL_DSP_SPEED, &v) < 0 || v < 8000 || v > 96000) v = rate;
    a->rate = v;
    /* FIONREAD straight to the kernel: works when padsp hands back the FIFO */
    int q = -1;
    a->fionread = syscall(SYS_ioctl, a->fd, FIONREAD, &q) == 0 && q >= 0;
    a->latency = 0.06;
    if ((e = getenv("MINIFIN_AUDIO_LATENCY_MS")) != NULL) a->latency = atof(e) / 1000.0;
    plog("audio: padsp fd %d, %d Hz, queue %s, latency %.0f ms", a->fd, a->rate,
         a->fionread ? "measured (FIONREAD)" : "estimated", a->latency * 1000);
    return a;
}

int aout_rate(const AOut *a) { return a->rate; }

int aout_write(AOut *a, const int16_t *pcm, int frames)
{
    const uint8_t *p = (const uint8_t *)pcm;
    size_t left = (size_t)frames * 4;
    while (left) {
        ssize_t n = a->p_write(a->fd, p, left);
        if (n < 0) {
            if (errno == EINTR || errno == EAGAIN) {
                usleep(2000);
                continue;
            }
            return -1;
        }
        p += n;
        left -= (size_t)n;
    }
    if (!a->fionread) {
        double now = now_sec();
        double queued = a->written - (now - a->t0);
        if (queued < 0) { /* ran dry: restart the estimate */
            a->t0 = now;
            a->written = 0;
        }
        a->written += (double)frames / a->rate;
    }
    return 0;
}

double aout_queued(AOut *a)
{
    if (a->fionread) {
        int q = 0;
        if (syscall(SYS_ioctl, a->fd, FIONREAD, &q) == 0 && q >= 0) return q / (a->rate * 4.0) + a->latency;
    }
    double queued = a->written - (now_sec() - a->t0);
    if (queued < 0) queued = 0;
    return queued + a->latency;
}

void aout_close(AOut *a)
{
    if (!a) return;
    if (a->fd >= 0) a->p_close(a->fd);
    /* libpadsp stays loaded: unloading an LD_PRELOAD-style shim is unsafe */
    free(a);
}
