# Changelog

All notable changes are listed here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [1.0.0] - 2026-11-03

### Added

- The first version. Finding servers on the Wi-Fi (Jellyfin's discovery broadcast) and on the tailnet, sign-in with Quick Connect or a password, and the token sealed with the device key.
- Home with Continue Watching, Next Up, Recently Added and the libraries; poster grids that fetch only the pages near the selection, with sort, a filter for unwatched videos and letter jumps; seasons and episodes; search; a page for each video with its audio and subtitle tracks.
- Playback in minifin-player (GPL-2.0-or-later, in `player/`, shipped in the zip) through a relay on the device, with Onion's ffplay when minifin-player is missing or can't start. The server transcodes to H.264 at up to 640x480 and AAC stereo, at 0.6, 1.2 or 2 Mbit/s. Seeking in minifin-player starts a new transcode at that point and stops the old one on the server. Start, progress, seeks and stop are reported to the server.
- Buttons in minifin-player: pause, skips of 10 s and a minute, volume, subtitles on and off, the audio track (X) and subtitle track (SELECT) without leaving the video, and the next or previous episode (R2 / L2, pressed twice).
- The next episode plays after a 10-second countdown when one ends, online or from the downloads.
- Downloads for offline viewing, with a free-space check, going on from the last keyframe after an interruption, and an offline view of them when the server can't be reached.
- Settings for the server and account, streaming quality, the subtitle default, the downloads folder, the player and Appearance.

[1.0.0]: https://github.com/zakijss/MiniFin/releases/tag/v1.0.0
