# Cross toolchain for minifin-player (Miyoo Mini Plus, Onion OS 4.4).
# Debian Buster has glibc 2.28, the same as the device, and gcc 8 for armhf.
#
#   docker build -t minifin-player-toolchain player
#   docker run --rm -v "$PWD/player:/src" -w /src minifin-player-toolchain make cross
#
# The image holds only the compiler. Headers and the device's own libraries
# are fetched into player/build/ by tools/fetch-deps.sh and
# tools/import-device-libs.sh, both of which check SHA-256 sums.
FROM debian:buster
RUN sed -i 's/deb\.debian\.org/archive.debian.org/g; s|security.debian.org|archive.debian.org/debian-security|g; /buster-updates/d' /etc/apt/sources.list \
 && apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      gcc-arm-linux-gnueabihf libc6-dev-armhf-cross make xz-utils ca-certificates wget \
 && rm -rf /var/lib/apt/lists/*
ENV CROSS=arm-linux-gnueabihf-
WORKDIR /src
