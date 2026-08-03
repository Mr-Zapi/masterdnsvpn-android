#!/usr/bin/env bash
# ==============================================================================
# Builds the Android library (mobile.aar) from the Go `mobile` package using
# gomobile, and drops it into android/app/libs/ so the Android app can link it.
#
# Requirements:
#   - Go (matching go.mod, currently go 1.25+)
#   - Android SDK + NDK installed, with ANDROID_HOME / ANDROID_NDK_HOME set
#   - Java 17
#
# Usage:
#   ./scripts/build-android-aar.sh
# ==============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

OUT_DIR="android/app/libs"
OUT_AAR="$OUT_DIR/mobile.aar"

echo "==> Ensuring golang.org/x/mobile is in the module graph"
# Newer Go toolchains require x/mobile to be a recorded dependency of the
# module before `gomobile bind` will run.
go get golang.org/x/mobile/bind
go get -tool golang.org/x/mobile/cmd/gobind

echo "==> Installing gomobile tooling"
go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest

# Make sure the freshly installed tools are on PATH.
export PATH="$(go env GOPATH)/bin:$PATH"

echo "==> gomobile init"
gomobile init

mkdir -p "$OUT_DIR"

echo "==> Building mobile.aar (android/arm64,arm,amd64)"
gomobile bind \
  -target=android/arm64,android/arm,android/amd64 \
  -androidapi 21 \
  -o "$OUT_AAR" \
  ./mobile

echo "==> Done: $OUT_AAR"
