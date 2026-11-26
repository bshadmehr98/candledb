#!/usr/bin/env bash
# Builds the server for every supported platform and packs each binary with
# the licence and README into an archive in release/, plus a checksum file.
# Usage: scripts/build-release.sh <version>      (for example 0.1.0)
set -euo pipefail

VERSION="${1:?usage: build-release.sh <version>}"
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}"
OUT="release"

rm -rf "$OUT"
mkdir -p "$OUT"

for platform in $PLATFORMS; do
  os="${platform%/*}"
  arch="${platform#*/}"
  name="candledb_${VERSION}_${os}_${arch}"
  stage="$OUT/$name"
  binary="candledb"
  [ "$os" = windows ] && binary="candledb.exe"

  echo "build $platform"
  mkdir -p "$stage"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o "$stage/$binary" ./cmd/candledb
  cp LICENSE README.md "$stage/"

  if [ "$os" = windows ]; then
    # python3 is used instead of "zip", which many machines do not have.
    (cd "$OUT" && python3 -m zipfile -c "$name.zip" "$name")
  else
    tar -C "$OUT" -czf "$OUT/$name.tar.gz" "$name"
  fi
  rm -rf "$stage"
done

# One file with a SHA-256 line per archive, verifiable with "sha256sum -c".
(cd "$OUT" && sha256sum candledb_* > checksums.txt)
echo "wrote $(ls "$OUT" | wc -l) files to $OUT/"
