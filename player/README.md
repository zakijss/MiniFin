# minifin-player

MiniFin's video player for the Miyoo Mini Plus on Onion OS 4.4. It decodes H.264 with the
SSD202D's hardware decoder (MI_VDEC), draws with the 2D engine (MI_GFX) and plays sound through
Onion's audioserver. If the hardware decoder can't be opened it decodes with libavcodec
instead, like ffplay.

At 640x480, 30 fps the hardware path uses about 13% of one CPU core. Software decoding of the
same clips takes 48 to 55%, and real films at 1.2 Mbit/s take more (MiyooFin's complaint).

## Usage

```
minifin-player --url URL|PATH [--start S] [--title TEXT] [--sub FILE.srt] [--duration D]
               [--next TITLE] [--prev TITLE] [--audio 'A|B' --audio-current I] [--subs 'Off|A' --subs-current I]
```

- `--url`: an `http://` URL (MiniFin's local relay) or a file path. MPEG-TS with H.264 and AAC is
  what it is built for; MP4 also plays (its H.264 goes through `h264_mp4toannexb`).
- `--start S`: the input already begins at video time S (the relay started the transcode there,
  or the offline copy starts at a keyframe). The player does not seek. Every time it reports is
  S plus the time since the input's first timestamp.
- `--title`: shown in the overlay.
- `--sub`: a SubRip file. Its times are from the start of the video, so they match `pos`.
- `--duration D` (optional, not in the original contract): the video's length, used for `dur`
  and the progress bar when the stream doesn't know its own length (a live transcode doesn't).
- `--next`, `--prev`: the titles of the next and previous episodes, for R2 and L2. Without them
  those buttons say there is none.
- `--audio`, `--subs`: the audio tracks and the subtitle choices, `|` between names, with the
  one playing (from 0) in `--audio-current` and `--subs-current`. X and SELECT step through them.
- Unknown options are skipped with a note on stderr, so a newer app doesn't make the player fail.

Run it with Onion's library path. `LD_PRELOAD=/mnt/SDCARD/miyoo/lib/libpadsp.so` is fine, but
not needed: the player `dlopen()`s libpadsp itself.

### stdout

One line per event, flushed as it's written. Times are seconds with one decimal (`pos 742.5`).

| Line | When |
|---|---|
| `dur <s>` | once the input is open; `0.0` when unknown and no `--duration` |
| `decoder hw` or `decoder sw` | right after `dur` |
| `pos <s>` | when the first frame is on screen, then about once a second (also while paused). During a seek it's the target |
| `paused 1` / `paused 0` | when A toggles pause |
| `end` | played to the end; exit 0 |
| `audio <i>`, `subtitle <i>` | before `quit`: play again from there with choice i of `--audio` / `--subs` |
| `next`, `prev` | before `quit`: play the next or previous episode |
| `quit <s>` | B, MENU, SIGTERM, SIGINT or SIGHUP, or after the lines above; exit 0 |
| `error <message>` | then exit 1 (or 2 for bad arguments) |

`error` cases: the input can't be opened, there's no video stream, no decoder works, the network
stalls for 60 s (`MINIFIN_STALL_SECONDS`), the hardware decoder stops producing frames for 5 s,
there isn't enough video memory for the resolution, or an HTTP stream ends more than 30 s
before the known duration (`stream ended early at X of Y s`, so a cut connection isn't taken
for the end of the film). A failed open exits within the open timeout (30 s,
`MINIFIN_OPEN_TIMEOUT`); a missing file fails at once.

Diagnostics, including stats every 10 s, go to stderr with a `minifin-player` prefix.
`MINIFIN_QUIET=1` turns them off.

### Buttons

Read from `/dev/input/event0`; the device isn't grabbed, so keymon still sees the volume and
power buttons.

| Button | Action |
|---|---|
| A | pause / play |
| Left / Right | -10 / +10 s |
| L1 / R1 | -60 / +60 s |
| Up / Down | volume +10 / -10 % (0 to 150 %, software gain; separate from the system volume) |
| Y | subtitles on / off ("No subtitles" without `--sub`) |
| X | next audio track, from `--audio` |
| SELECT | next subtitle choice, from `--subs` (Off first) |
| R2 / L2 | next / previous episode: the first press names it, a second within 3 s plays it |
| START | overlay on / off |
| B, MENU | stop: prints `quit <pos>` |

A track choice is applied 1.2 s after the last X or SELECT, so several presses step past
tracks without restarting each time. MiniFin then starts the video again from the same place with
that track; the server transcodes the new choice.

Left, Right, L1, R1, Up and Down repeat while held, at most every 150 ms. Seeks add up: pressing
Right three times quickly is one seek to +30 s. The seek starts 400 ms after the last press for
HTTP (each one is a new transcode) and 150 ms for files. While it reopens, the overlay shows
"Seeking to 12:34".

The overlay (title on top; play state, time, progress, volume and subtitle state at the bottom)
appears and disappears at once, with no animation. It also shows while paused, while seeking,
while loading, and for 3 s after a seek. Short notices ("Volume 80%", "Buffering...")
appear in the top centre.

### Seeking

- HTTP input: the player opens the same URL again with `?t=<seconds>` (or `&t=`), the contract
  with MiniFin's relay. Before that it sends one GET and reads the reply's `X-MiniFin-Start`
  header, which says where the new stream really begins. That becomes the clock base; when it is
  earlier than t (an offline copy starting at the keyframe before t), frames and sound before t
  are skipped. Without the header the stream is assumed to begin at t. The reopen gives libavformat
  the known demuxer and reads only 256 KB / 0.5 s to find the streams. Against
  `tools/test-relay.py` it took 210 to 290 ms on the Mac.
- File input: libavformat seeks to 3 s before the target, and the player skips to the target
  (keyframes in MiniFin's transcodes are 2 to 3 s apart).

## How it works

```
reader thread    libavformat -> packet queues (timestamps rewritten to absolute microseconds)
video thread     MI_VDEC -> NV12 -> YUV 4:2:2 copy (NEON) -> frame pool -> MI_GFX blit to fb0
audio thread     libavcodec AAC -> swresample s16 stereo 48 kHz -> libpadsp /dev/dsp -> audioserver
main thread      buttons, overlay state, stdout lines
```

What the device taught me, in the order I found it:

1. **MI_VDEC decodes H.264 at about 270 to 300 fps at 640x480**, using 7% CPU for the feeding.
   Opening the decoder takes 5 ms; the very first open after boot took 2.1 s.
2. **Output order.** In the default low-latency mode, VDEC hands out plain NV12 frames in decode
   order, each with its correct timestamp. With low latency off it hands out metadata buffers
   meant for the bound display pipeline (MI_DIVP), not frames. So the player keeps low latency
   on and sorts frames itself, holding back `has_b_frames` of them (2 for x264's defaults). If a
   frame still arrives too late it's dropped and one more is held.
3. **MI_DIVP is unusable on this firmware.** Its driver fails `request_irq` at init, it rejects
   VDEC's metadata buffers ("Input width/height is too small"), and `MI_DIVP_StretchBuf` times
   out after 105 ms ("Process SclTask Timeout"). So there's no hardware NV12 to RGB through DIVP.
4. **MI_GFX does convert, scale and rotate**, but only from YUV 4:2:2, and it reads the bytes as
   Y0 V Y1 U. The player repacks NV12 into that with NEON (1.3 to 1.75 ms a frame at 640x480;
   pictures above 640x480 are decimated by a whole factor while repacking). One `MI_GFX_BitBlit`
   then converts, scales to the letterbox and rotates 180 degrees for the upside-down panel,
   straight into the framebuffer's back buffer. A second blit alpha-blends the overlay, then
   `FBIOPAN_DISPLAY` flips. The CPU converting NV12 to RGB instead takes 14 ms a frame.
5. **The decoder never waits for you.** When its output queue is full, MI_SYS overwrites the
   oldest frame. The first version fed a whole file in and got 2 of 598 frames. Now each frame is
   copied out at once into a pool of 5 buffers, and only queue depth minus one packet is in
   flight (4 at 640x480).
6. **Memory is tight.** The MMA heap is 21 MB and the framebuffer takes 4 of it. At 720p VDEC
   needs about 12 MB for its own buffers plus 1.4 MB per output frame. So above 640x480 the player
   uses 3 reference frames, an output queue of 3 and a smaller pool. A slot that can't get memory
   is disabled before a frame is taken, so no frame is lost to a failed allocation.
7. **Never `MI_VDEC_ResetChn`.** Stop/Reset/Start leaves the channel producing nothing. When the
   process then exits, the kernel's VDEC cleanup loops forever ("Wait Close") and only a reboot
   helps. A seek destroys and recreates the channel instead (1 to 7 ms).
8. **Don't `kill -9` a player in trouble.** After a clean run the kernel destroys a killed
   process's VDEC channel itself. But after a run where VDEC couldn't allocate, `kill -9` left the
   kernel's `mi_sys` cleanup unfinished, and the next MainUI hung in uninterruptible sleep when it
   connected. The player now ends itself with an `error` line when the decoder stalls, and SIGTERM
   gives a clean shutdown. The app should stop it with SIGTERM.
9. **Sound.** libpadsp (Onion's `/dev/dsp` shim, also used for RetroArch) is `dlopen()`ed and its
   `open`/`ioctl`/`write` are called directly. It forwards to audioserver through a FIFO, resized
   to 4 KB (21 ms). libpadsp prints notes on stdout when it opens, so stdout points at stderr for
   that call. The queue is measured with a raw `FIONREAD` on the FIFO; the audio clock is the end
   of what was written minus what's queued, minus a fixed 60 ms for audioserver and the DMA
   (`MINIFIN_AUDIO_LATENCY_MS`, an estimate: I couldn't measure the speaker).
10. **Clock.** Audio is the master. The fallback is a wall clock, used when there's no audio or
    after 5 s without sound (an audio track that ends early). While audio is starved (buffering)
    the clock stops with it, so video doesn't run ahead. Video frames are shown up to a quarter
    frame early. One more than a frame late, a frame is dropped if a newer one is ready, but at
    least one frame is shown every 100 ms.
11. **Screen.** `/dev/fb0` is opened with a raw `openat` syscall, because libpadsp, when preloaded,
    wraps `open()` and handles `/dev/fb0` itself. On exit both buffers are cleared and the display
    is panned back to the first, so MainUI starts from a black screen.

If MI_GFX can't be opened, everything is drawn by the CPU (libswscale to BGRA, copied upside
down, overlay blended in), and the hardware decoder isn't used. `MINIFIN_DECODER=sw` forces
libavcodec, and `MINIFIN_GFX=0` forces CPU drawing.

## Building

Docker and the toolchain image (Debian Buster: glibc 2.28 and gcc 8 for armhf, like the device):

```
make docker                         # -> build/minifin-player (stripped), build/probe
make test                           # host tests, any Unix with cc
tools/import-device-libs.sh miyoo   # optional: link against the device's own .so files
```

`make docker` runs `tools/fetch-deps.sh`, which downloads and checksums the FFmpeg 2.4.14
tarball (headers only: configure and `make install-headers`) and the MI SDK headers from
steward-fu/sdl2 at a pinned commit. Then `tools/build-linkonly-libs.sh` makes a link-only
stand-in for each device library missing from `build/sysroot/lib`: FFmpeg's five libraries built
from the same tarball (same sonames and symbol versions as Onion's), and the MI libraries as empty
functions under the device's sonames. That is how CI builds the release. The result imports the
same symbols, with the same versions, from the same libraries as a build against the device's own
files; only the order of the version table differs. At run time the device's own FFmpeg is used; the
binary needs GLIBC_2.4 and GLIBC_2.17 symbols only. A clean `make docker` takes about 2 minutes
and gives the same binary each time.

## Tests

- `make test`: the SubRip parser (BOM, CRLF, markup, overlapping and unsorted cues, missing blank
  lines, Windows-1252 files), command line parsing and the stdout lines, the clock maths
  (including 10 simulated minutes against a sound card running 0.05% fast), the `?t=` URL and the
  `X-MiniFin-Start` header, and the overlay layout. The overlay test writes
  `build/host/osd-*.ppm` to look at.
- `MEDIA=dir tools/run-live-test.sh`: `demux.c` against `tools/test-relay.py` in containers
  (a relay with the same `?t=` and `X-MiniFin-Start` behaviour): live transcode, resumed
  transcode, offline copy that starts before t, and a local file. All four pass.
- `probe/probe.c`: the experiments behind the "How it works" list (decode speed, output order,
  conversion options, DIVP).
- `tools/device/`: what I used on the device. `go.sh` starts `run.sh` through Onion
  (`/tmp/cmd_to_run.sh`, then MainUI is killed), only when nothing else runs. `run.sh` reads
  `args` and `env`, and has a watchdog (SIGTERM, SIGKILL 5 s later). `t.sh` drives a run from the
  Mac with button presses and screenshots through an `mmp` helper (`MMP=path`).

## Measurements

Miyoo Mini Plus, Onion OS 4.4, files on the SD card, charging. 2026-11-02.

| Clip | Decoder | Frames shown / dropped | CPU (one core) | RSS | A/V: mean, sd, worst late |
|---|---|---|---|---|---|
| 640x480 30 fps, 20 s, synthetic | hw | 597 / 1 | 12 to 13% | 9.7 MB | 2.7 ms, 4.2 ms, 29 ms |
| 640x480 30 fps, 37 s, with subtitles | hw | 1117 / 0 | 13% | 9.5 MB | 2.8 ms, 4.1 ms, 12 ms |
| Big Buck Bunny 640x360 24 fps, 1.2 Mbit/s, 20 s | hw | 461 / 0 | 10 to 11% | 10.6 MB | 4.9 ms, 3.7 ms, 10 ms |
| same | sw | 466 / 0 | 48 to 55% | 16.4 MB | 3.8 ms, 4.5 ms, 15 ms |
| 640x480 24 fps MP4, 20 s | hw | 476 / 0 | 10 to 11% | 9.3 MB | 5.2 ms, 3.6 ms, 11 ms |
| 640x480 24 fps, 6 damaged 2 KB blocks | hw | 438 / 19 | 12% | 9.7 MB | 3.9 ms, 10.8 ms, 125 ms |
| 640x480 24 fps cut at 45%, mid-packet | hw | 212 / 0, `end` at 9.1 s | | | |

"A/V" is the shown frame's timestamp minus the audio clock at the moment it was shown, so it
measures the player's scheduling. How well the clock matches the speaker depends on the 60 ms
latency estimate, which I couldn't measure without a microphone.

- Startup, file, until the first frame is on screen and the first `pos` line: 180 to 275 ms. A
  clip cut mid-GOP: 1.2 s (it waits for a keyframe).
- Seek in a file: libavformat 7 to 47 ms, decoder channel recreated in 1 to 7 ms. Then the frame
  at the target shows after the 150 ms press coalescing.
- Software path, same controls: seeks 15 to 40 ms; pause, quit and subtitles work.
- Exit: `end` within 0.4 s of the last frame, `quit` at once. After each normal exit MainUI came back and
  `/proc/mi_modules/mi_vdec` held only its static files.

## Not done, and open problems

- **Not measured on the device:** 10 minutes of playback (the 11-minute test clip is on the card
  but was never run), A/V drift over 10 minutes, the stress runs (50 seeks, 100 pauses,
  30 starts and stops), HTTP playback on the device, a stalled network, and 720p after the
  memory fix. All this was planned once the device was back from its second problem, but it
  stopped again (see 8. above), and the rule was to stop there.
- **720p** ran out of MMA memory before the fix in 6.; the fix is untested. MiniFin asks the
  server for 640x480 at most.
- **Over HTTP** only the demuxer has been tested (in containers, against the test relay). The
  device's Wi-Fi was at -80 dBm and 13 to 140 KB/s during these tests: too slow for a
  1.2 Mbit/s stream from the Mac. The Mac's firewall blocks incoming connections, so the
  plan was a reverse SSH tunnel to the device's localhost, the same shape of URL as the relay's.
- **The sound card's latency** (60 ms) is an estimate.
- **Pause** stops writing sound at once, but the 4 KB FIFO and audioserver still play out about
  80 ms.
- **The first frame of a file** is sometimes dropped (597 of 598 shown): VDEC's first decode-order
  frame can arrive after the next one has been shown.
- **MMA when the app runs:** MainUI isn't running then, but if MiniFin's Go app holds MMA memory,
  720p has even less room.

## Licence

GPL-2.0-or-later (LICENSE). It links Onion's FFmpeg, which was built with `--enable-gpl
--enable-nonfree`. See NOTICE for what it links and where the headers come from.
