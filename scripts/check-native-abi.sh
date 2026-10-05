#!/bin/sh
set -eu
: "${1:?Pass the Linux shared library path}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
"${CC:-cc}" -D_GNU_SOURCE -Wall -Wextra -Werror scripts/check-native-abi.c -ldl -o "$tmp/check-native-abi"
"$tmp/check-native-abi" "$1"
