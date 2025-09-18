set -euo pipefail
SRC="talos_enc"
DST="talos_dec"

mkdir -p "$DST"

find "$SRC" -type f -print0 |
while IFS= read -r -d '' src; do
  rel="${src#$SRC/}"
  dir="$(dirname "$rel")"
  base="$(basename "$rel")"
  out_base="${base//.enc/}"
  out="$DST/$dir/$out_base"
  mkdir -p "$(dirname "$out")"
  sops -d "$src" > "$out"
  echo "[dec] $src -> $out"
done

