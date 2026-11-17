#!/bin/sh
# Builds self-contained ffmpeg and ffprobe for the app into tools/bin.
#
# The binaries are LGPL-only (no --enable-gpl, no external libraries), linked
# statically against FFmpeg's own libraries and dynamically only against
# macOS system libraries, so they can be shipped inside the app. Only the
# decoders, demuxers and filters are complete; the only encoders and muxers
# are the raw ones the app uses to read pictures and sound into memory.
set -eu
cd "$(dirname "$0")/.."

version=9.0.2
# Checksum of the release tarball, whose signature was verified against the
# FFmpeg release key FCF9 86EA 15E6 E293 A564 4F10 B432 2F04 D676 58D8.
sha256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e
url="https://ffmpeg.org/releases/ffmpeg-$version.tar.xz"
work=tools/ffmpeg
src="$work/ffmpeg-$version"

mkdir -p "$work" tools/bin
if [ ! -d "$src" ]; then
  archive="$work/ffmpeg-$version.tar.xz"
  [ -f "$archive" ] || curl -fL --retry 3 -o "$archive" "$url"
  got=$(shasum -a 256 "$archive" | cut -d' ' -f1)
  if [ "$got" != "$sha256" ]; then
    echo "checksum mismatch for $archive: $got" >&2
    exit 1
  fi
  tar -xJf "$archive" -C "$work"
fi

cd "$src"
./configure \
  --prefix="$PWD/../install" \
  --cc=clang \
  --extra-cflags="-mmacosx-version-min=11.0" \
  --extra-ldflags="-mmacosx-version-min=11.0" \
  --enable-static --disable-shared \
  --disable-autodetect --enable-zlib \
  --disable-ffplay --disable-doc --disable-debug \
  --disable-network --disable-devices \
  --disable-encoders --enable-encoder=rawvideo,pcm_s16le \
  --disable-muxers --enable-muxer=rawvideo,pcm_s16le,null \
  --disable-bsfs
make -j"$(sysctl -n hw.ncpu)" ffmpeg ffprobe
cp ffmpeg ffprobe ../../bin/
cp COPYING.LGPLv2.1 ../../bin/FFMPEG-COPYING.LGPLv2.1
cat > ../../bin/FFMPEG-README.txt <<TXT
This app includes ffmpeg and ffprobe $version from the FFmpeg project
(https://ffmpeg.org), licensed under the GNU LGPL version 2.1 or later; the
licence text is in FFMPEG-COPYING.LGPLv2.1.

They were built unmodified from $url
with scripts/build-ffmpeg.sh in this app's source repository, which lists the
exact configure options.
TXT
cd - >/dev/null
echo "Built tools/bin/ffmpeg and tools/bin/ffprobe ($version, LGPL)"
