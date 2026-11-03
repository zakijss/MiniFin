#!/bin/sh
# Packs the app into dist/: minifin-V.zip, laid out to unzip onto the SD
# card root; app.json, its entry in MiniMart's catalogue; and SHA256SUMS.
# Settings, the sign-in and downloads aren't included, so unzipping an
# update keeps yours. minifin-player (player/build/minifin-player, built by
# player/Makefile) goes in with its GPL licence and NOTICE; without it the
# zip still works and plays with Onion's ffplay (REQUIRE_PLAYER=1 makes
# that an error).
#   VERSION=1.0.0 ./scripts/build.sh && ./scripts/package.sh 1.0.0
set -e
cd "$(dirname "$0")/.."
V=${1:?usage: package.sh VERSION}
V=${V#v}
ZIP=minifin-$V.zip
for b in minifin; do
    LC_ALL=C grep -aq "minideck-version=$V" "dist/$b" ||
        { echo "dist/$b isn't version $V: run VERSION=$V scripts/build.sh" >&2; exit 1; }
done

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
A="$STAGE/App/MiniFin"
mkdir -p "$A/licenses"
cp app/* "$A/"
cp dist/minifin "$A/"
PLAYER=player/build/minifin-player
[ -f dist/minifin-player ] && PLAYER=dist/minifin-player
if [ -f "$PLAYER" ]; then
    cp "$PLAYER" "$A/minifin-player"
    chmod 755 "$A/minifin-player"
    cp player/LICENSE "$A/licenses/minifin-player-LICENSE"
    cp player/NOTICE "$A/licenses/minifin-player-NOTICE"
elif [ "${REQUIRE_PLAYER:-0}" = 1 ]; then
    echo "no $PLAYER: build it with make -C player" >&2; exit 1
else
    echo "note: no $PLAYER; the zip plays with ffplay only" >&2
fi
echo "$V" > "$A/version.txt"
cp LICENSE "$A/licenses/minifin-LICENSE"
cp licenses/* "$A/licenses/"
rm -f "dist/$ZIP"
(cd "$STAGE" && COPYFILE_DISABLE=1 zip -qrX "$OLDPWD/dist/$ZIP" .)

sum=$(shasum -a 256 "dist/$ZIP" | cut -d' ' -f1)
size=$(wc -c < "dist/$ZIP" | tr -d ' ')
sed -e "s/__VERSION__/$V/g" -e "s/__SHA256__/$sum/" -e "s/__SIZE__/$size/" \
    -e "s/__RELEASED__/$(date -u +%Y-%m-%d)/" store.json > dist/app.json
(cd dist && shasum -a 256 "$ZIP" app.json > SHA256SUMS && cat SHA256SUMS)
