#!/bin/sh
# Build pryon_wake for the Echo (ARMv7, Android API 22) in the pinned compiler
# image, the same toolchain as the server. Output: device/tools/pryon_wake/pryon_wake
# Needs no Amazon file: libpryon.so is dlopen()ed at run time.
set -eu
ROOT=$(git rev-parse --show-toplevel)
docker run --rm --entrypoint bash \
    -v "$ROOT/device/tools/pryon_wake":/sdk \
    echomuse-compiler \
    -c 'cd /sdk && $CC -O2 -Wall -Wextra -fPIE -pie -std=gnu11 -o pryon_wake pryon_wake.c -ldl'
ls -l "$ROOT/device/tools/pryon_wake/pryon_wake"
