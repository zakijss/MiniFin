# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [1.1.1] - 2026-11-12

### Changed

- Reaches the WireGuard and NetBird networks MiniScale 1.2 can join, through its proxy (MiniDeck 1.1.0).

## [1.1.0] - 2026-11-12

### Added

- Mark a video watched or unwatched: X on its page, Y on a season's episodes. It clears the resume point, as Jellyfin's own apps do, and the open pages update.

### Changed

- Home shows Y Refresh in the footer; Y already reloaded the rails.
- Go 1.27.4 and updated dependencies.

## [1.0.4] - 2026-11-08

### Changed

- Empty libraries and seasons say that videos added in Jellyfin show up there.
- A download with a missing part says to delete it and download it again.
- "Can't play" says to restart MiniFin when the local relay won't open.
- Grey and red small text is easier to read (MiniDeck 1.0.3).

## [1.0.3] - 2026-11-07

### Fixed

- A video from a slow server no longer stops with "the video decoder stopped working" after a few seconds. When the server can't keep up, the hardware decoder can stop handing out pictures while it still has some; minifin-player now restarts it at the next keyframe, as a seek does, so the picture freezes for about 5 s and goes on. It gives up only when three restarts in a row bring nothing. Why the decoder stalls on such a stream is not yet known.

## [1.0.2] - 2026-11-07

### Fixed

- minifin-player no longer crashes as a video starts. Onion's audio library (libpadsp) jumps to a null pointer when its first call opens /dev/dsp, which is always the case when it is loaded with dlopen() rather than preloaded; the player now opens /dev/null through it first. Before this, every video fell back to ffplay, or went back to its page when the crash came after playback had started.
- A long audio or subtitle track name on a video's page is cut with an ellipsis instead of running over the row's label and out of the menu (MiniDeck 1.0.2).

### Changed

- minifin-player writes the crash address to its log on a crash.
- The README screenshots are from a real device and server.

## [1.0.1] - 2026-11-03

### Fixed

- A poster whose header claims an enormous size is refused on the device too. The size check multiplied in 32 bits there and could wrap to a small number, letting a bad server's picture use up the memory.

## [1.0.0] - 2026-11-03

### Added

- The first version. Finding servers on the Wi-Fi (Jellyfin's discovery broadcast) and on the tailnet, sign-in with Quick Connect or a password, and the token sealed with the device key.
- Home with Continue Watching, Next Up, Recently Added and the libraries; poster grids that fetch only the pages near the selection, with sort, a filter for unwatched videos and letter jumps; seasons and episodes; search; a page for each video with its audio and subtitle tracks.
- Playback in minifin-player (GPL-2.0-or-later, in `player/`, shipped in the zip) through a relay on the device, with Onion's ffplay when minifin-player is missing or can't start. The server transcodes to H.264 at up to 640x480 and AAC stereo, at 0.6, 1.2 or 2 Mbit/s. Seeking in minifin-player starts a new transcode at that point and stops the old one on the server. Start, progress, seeks and stop are reported to the server.
- Buttons in minifin-player: pause, skips of 10 s and a minute, volume, subtitles on and off, the audio track (X) and subtitle track (SELECT) without leaving the video, and the next or previous episode (R2 / L2, pressed twice).
- The next episode plays after a 10-second countdown when one ends, online or from the downloads.
- Downloads for offline viewing, with a free-space check, going on from the last keyframe after an interruption, and an offline view of them when the server can't be reached.
- Settings for the server and account, streaming quality, the subtitle default, the downloads folder, the player and Appearance.

[Unreleased]: https://github.com/zakijss/MiniFin/compare/v1.1.1...HEAD
[1.1.1]: https://github.com/zakijss/MiniFin/compare/v1.1.0...v1.1.1
[1.1.0]: https://github.com/zakijss/MiniFin/compare/v1.0.4...v1.1.0
[1.0.4]: https://github.com/zakijss/MiniFin/compare/v1.0.3...v1.0.4
[1.0.3]: https://github.com/zakijss/MiniFin/compare/v1.0.2...v1.0.3
[1.0.2]: https://github.com/zakijss/MiniFin/compare/v1.0.1...v1.0.2
[1.0.1]: https://github.com/zakijss/MiniFin/compare/v1.0.0...v1.0.1
[1.0.0]: https://github.com/zakijss/MiniFin/releases/tag/v1.0.0
