#!/usr/bin/env sh
set -eu

# Release metadata used for versioned archive filenames.
VERSION=v1.0.5
DARWIN_PACKAGE=pdf-tool-${VERSION}-darwin-amd64
WINDOWS_PACKAGE=pdf-tool-${VERSION}-windows-amd64

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
CACHE_DIR=${GOCACHE:-${TMPDIR:-$HOME/.cache}/go-build}
ARCHIVE_DIR="$ROOT_DIR/dist/archive"
SOURCE_ARCHIVE="$ROOT_DIR/pdf-tool-${VERSION}.zip"

mkdir -p "$ROOT_DIR/dist/darwin" "$ROOT_DIR/dist/win" "$CACHE_DIR" "$ARCHIVE_DIR"

export GOTELEMETRY=off
export GOCACHE="$CACHE_DIR"

cd "$ROOT_DIR"

echo "Building macOS binary..."
go build -o dist/darwin/pdf-tool .

echo "Building Windows amd64 binary..."
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 CC=/opt/homebrew/bin/x86_64-w64-mingw32-gcc \
  go build -o dist/win/pdf-tool.exe .

# Build self-contained release archives in a temporary staging directory.
# This keeps the public release package separate from the compiled binaries,
# while still letting both formats reuse the same build outputs.
STAGE_DIR="$ROOT_DIR/dist/package-tmp"
rm -rf "$STAGE_DIR"
mkdir -p "$STAGE_DIR/$DARWIN_PACKAGE" "$STAGE_DIR/$WINDOWS_PACKAGE"

cp "$ROOT_DIR/dist/darwin/pdf-tool" "$STAGE_DIR/$DARWIN_PACKAGE/"
cp "$ROOT_DIR/README.md" "$STAGE_DIR/$DARWIN_PACKAGE/"
cp "$ROOT_DIR/VERSION.md" "$STAGE_DIR/$DARWIN_PACKAGE/"

cp "$ROOT_DIR/dist/win/pdf-tool.exe" "$STAGE_DIR/$WINDOWS_PACKAGE/"
cp "$ROOT_DIR/README.md" "$STAGE_DIR/$WINDOWS_PACKAGE/"
cp "$ROOT_DIR/VERSION.md" "$STAGE_DIR/$WINDOWS_PACKAGE/"

tar -C "$STAGE_DIR" -czf "$ARCHIVE_DIR/$DARWIN_PACKAGE.tar.gz" "$DARWIN_PACKAGE"
(cd "$STAGE_DIR" && zip -qr "$ARCHIVE_DIR/$WINDOWS_PACKAGE.zip" "$WINDOWS_PACKAGE")

# Build a root-level source archive that matches the older v1.0.0 layout.
# This package keeps only the key source and manifest files so it stays small,
# easy to inspect, and consistent with the previous release artifact.
zip -qj "$SOURCE_ARCHIVE" \
  "$ROOT_DIR/go.mod" \
  "$ROOT_DIR/go.sum" \
  "$ROOT_DIR/README.md" \
  "$ROOT_DIR/VERSION.md" \
  "$ROOT_DIR/main.go"

rm -rf "$STAGE_DIR"

echo "Done."
echo "  $ROOT_DIR/dist/darwin/pdf-tool"
echo "  $ROOT_DIR/dist/win/pdf-tool.exe"
echo "  $ARCHIVE_DIR/$DARWIN_PACKAGE.tar.gz"
echo "  $ARCHIVE_DIR/$WINDOWS_PACKAGE.zip"
echo "  $SOURCE_ARCHIVE"