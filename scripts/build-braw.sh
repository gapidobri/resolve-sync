#!/bin/sh
# Builds the Blackmagic RAW helper into tools/bin/brawframes.
#
# Needs the Blackmagic RAW SDK installed (free from Blackmagic Design's
# developer page). Its headers may not be redistributed, so they are used
# from where the installer puts them and are not part of this repository.
set -eu
cd "$(dirname "$0")/.."
sdk="${BRAW_SDK:-/Applications/Blackmagic RAW/Blackmagic RAW SDK}"
if [ ! -f "$sdk/Mac/Include/BlackmagicRawAPI.h" ]; then
  echo "Blackmagic RAW SDK not found at $sdk (set BRAW_SDK)" >&2
  exit 1
fi
mkdir -p tools/bin
clang++ -std=c++17 -O2 -Wno-deprecated-declarations \
  -I"$sdk/Mac/Include" -I"$sdk/Samples/Common" \
  braw/brawframes.cpp "$sdk/Mac/Include/BlackmagicRawAPIDispatch.cpp" \
  -framework CoreFoundation -o tools/bin/brawframes
echo "Built tools/bin/brawframes"
