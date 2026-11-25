#!/bin/sh
# Copies Keybar's logs into ./logs (git-ignored), named after this computer.
src="${XDG_CACHE_HOME:-$HOME/.cache}/keybar"
dst="$(dirname "$0")/logs"
host="$(hostname)"
mkdir -p "$dst"
found=0
for f in keybar.log keybar.prev.log; do
  if [ -f "$src/$f" ]; then
    cp "$src/$f" "$dst/$host-$f" && echo "Copied $f to logs/$host-$f"
    found=1
  fi
done
[ "$found" = 1 ] || echo "No logs found in $src - has Keybar run on this computer?"
