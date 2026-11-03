# minifin-player: hardware H.264 player for the Miyoo Mini Plus (Onion OS).
#
#   make docker        build the toolchain image and cross-compile build/minifin-player
#   make test          build and run the host tests (any Unix with cc)
#   make cross         cross-compile (inside the toolchain container)
#
# "make cross" fetches the FFmpeg and MI SDK headers (tools/fetch-deps.sh) and
# makes link-only stand-ins for any device library missing from
# build/sysroot/lib (tools/build-linkonly-libs.sh). tools/import-device-libs.sh
# copies the device's own libraries there instead, over SSH.

CROSS   ?= arm-linux-gnueabihf-
IMAGE   ?= minifin-player-toolchain
B       := build
SYSROOT := $(B)/sysroot

PLAYER_SRCS := src/main.c src/player.c src/demux.c src/video_hw.c src/video_sw.c src/display.c \
               src/audio.c src/osd.c src/srt.c src/proto.c src/clock.c src/font.c src/input.c src/urlutil.c src/log.c
PLAYER_HDRS := $(wildcard src/*.h) src/third_party/font8x8.h
PURE_SRCS   := src/srt.c src/proto.c src/clock.c src/font.c src/osd.c src/urlutil.c

ARM_CFLAGS  := -std=gnu99 -O2 -g -Wall -Wextra -Wno-unused-parameter -Wno-deprecated-declarations \
               -Wno-missing-field-initializers -D_GNU_SOURCE -D_FILE_OFFSET_BITS=64 \
               -mcpu=cortex-a7 -mfpu=neon-vfpv4 -mfloat-abi=hard \
               -I$(B)/include -I$(B)/include/mi -Isrc
# Link against the device's own libraries; they are loaded from Onion's
# LD_LIBRARY_PATH at run time (the rpath is a fallback for a bare shell).
ARM_LDFLAGS := -L$(SYSROOT)/lib -Wl,-rpath-link,$(SYSROOT)/lib -Wl,--allow-shlib-undefined \
               -Wl,-rpath,/config/lib:/customer/lib:/mnt/SDCARD/.tmp_update/lib/parasyte
ARM_LIBS    := -lavformat -lavcodec -lswresample -lswscale -lavutil \
               -lmi_sys -lmi_vdec -lmi_divp -lmi_gfx -lmi_common -ldl -lpthread -lm

.PHONY: all cross deps docker image test clean
all: test

deps:
	sh tools/fetch-deps.sh
	sh tools/build-linkonly-libs.sh

cross: deps $(B)/minifin-player $(B)/probe

$(B)/minifin-player: $(PLAYER_SRCS) $(PLAYER_HDRS)
	$(CROSS)gcc $(ARM_CFLAGS) $(PLAYER_SRCS) -o $@.debug $(ARM_LDFLAGS) $(ARM_LIBS)
	$(CROSS)strip -o $@ $@.debug

$(B)/probe: probe/probe.c
	$(CROSS)gcc $(ARM_CFLAGS) $< -o $@ $(ARM_LDFLAGS) $(ARM_LIBS)

image:
	docker build -t $(IMAGE) -f Dockerfile tools

docker: image
	docker run --rm -v "$(CURDIR):/src" -w /src $(IMAGE) make cross

# Host tests: the parts with no device dependency.
HOST_CC     ?= cc
HOST_CFLAGS := -std=gnu99 -O1 -g -Wall -Wextra -Wno-unused-parameter -Isrc -fsanitize=address,undefined
TESTS       := $(B)/host/test_srt $(B)/host/test_proto $(B)/host/test_clock $(B)/host/test_osd $(B)/host/test_url

test: $(TESTS)
	@for t in $(TESTS); do echo "== $$t"; $$t || exit 1; done
	@echo "all host tests passed"

$(B)/host/test_%: tests/test_%.c $(PURE_SRCS) $(PLAYER_HDRS) tests/check.h
	@mkdir -p $(B)/host
	$(HOST_CC) $(HOST_CFLAGS) $< $(PURE_SRCS) -o $@ -lm

clean:
	rm -rf $(B)/host $(B)/minifin-player $(B)/minifin-player.debug $(B)/probe
