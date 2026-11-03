/* Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#include "input.h"

#include <fcntl.h>
#include <linux/input.h>
#include <unistd.h>

#include "log.h"

int input_open(void)
{
    int fd = open("/dev/input/event0", O_RDONLY | O_NONBLOCK | O_CLOEXEC);
    if (fd < 0) plog("input: no /dev/input/event0");
    return fd;
}

int input_next(int fd, int *code, int *value)
{
    struct input_event ev;
    while (fd >= 0 && read(fd, &ev, sizeof ev) == (ssize_t)sizeof ev) {
        if (ev.type != EV_KEY) continue;
        *code = ev.code;
        *value = ev.value;
        return 1;
    }
    return 0;
}

void input_close(int fd)
{
    if (fd >= 0) close(fd);
}
