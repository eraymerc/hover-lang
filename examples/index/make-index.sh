#!/bin/sh
# Build an index archive from this directory.
#
# An index is just an archive whose root contains a packages/ directory. That
# is the entire format — no service, no database, no accounts. hover-lang.org
# will publish exactly one file like this.
#
# Usage:
#   ./make-index.sh [output.tar.gz]
#
# The archive wraps everything in a single top-level directory, which is what
# GitHub's and GitLab's auto-generated tag archives do too. hpm strips one
# wrapping directory on extraction, so both layouts work identically.

set -eu

OUT="${1:-index.tar.gz}"
HERE="$(cd "$(dirname "$0")" && pwd)"
NAME="hover-index"

if [ ! -d "$HERE/packages" ]; then
    echo "no packages/ directory next to $0" >&2
    exit 1
fi

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/$NAME"
cp -R "$HERE/packages" "$STAGE/$NAME/packages"

case "$OUT" in
    *.tar.zst) tar -C "$STAGE" --zstd -cf "$OUT" "$NAME" ;;
    *.tar.gz|*.tgz) tar -C "$STAGE" -czf "$OUT" "$NAME" ;;
    *) echo "output must end in .tar.gz or .tar.zst (got $OUT)" >&2; exit 1 ;;
esac

echo "wrote $OUT"
echo "  $(find "$HERE/packages" -name '*.toml' | wc -l | tr -d ' ') package entr(y/ies)"
