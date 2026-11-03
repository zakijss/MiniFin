/* Buttons: /dev/input/event0 (16-byte struct input_event on this kernel).
 * Copyright (C) 2026 Zaki. GPL-2.0-or-later. */
#ifndef MINIFIN_INPUT_H
#define MINIFIN_INPUT_H

enum {
    BTN_UP_ = 103, BTN_DOWN_ = 108, BTN_LEFT_ = 105, BTN_RIGHT_ = 106,
    BTN_A_ = 57, BTN_B_ = 29, BTN_X_ = 42, BTN_Y_ = 56,
    BTN_L1_ = 18, BTN_R1_ = 20, BTN_L2_ = 15, BTN_R2_ = 14,
    BTN_SELECT_ = 97, BTN_START_ = 28, BTN_MENU_ = 1,
};

int input_open(void); /* -1 if there is no event0 */
/* Next key event: 1 with code and value (1 press, 2 repeat, 0 release);
 * 0 when there is none waiting. */
int input_next(int fd, int *code, int *value);
void input_close(int fd);

#endif
