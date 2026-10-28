#!/bin/sh
# Check every shipped binary, not just the platform declared by the image.
set -eu
arch=$1
shift
case "$arch" in
  amd64) expected='x86-64' ;;
  arm64) expected='ARM aarch64' ;;
  *) echo "unsupported target architecture: $arch" >&2; exit 1 ;;
esac
[ "$#" -gt 0 ] || { echo 'no binaries supplied' >&2; exit 1; }
for binary do
  description=$(file -b "$binary")
  printf '%s: %s\n' "$binary" "$description"
  case "$description" in
    "ELF 64-bit LSB executable, $expected,"*) ;;
    *) echo "wrong architecture or executable format: $binary (expected $arch)" >&2; exit 1 ;;
  esac
done
