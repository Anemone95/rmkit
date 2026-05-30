#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
MIRROR="${DEBIAN_MIRROR:-https://deb.debian.org/debian}"
SUITE="${DEBIAN_SUITE:-bookworm}"
ARCH="${DEBIAN_ARCH:-arm64}"
OUT_DIR="${1:-$REPO_ROOT/dist/poppler-aarch64}"
WORKDIR=$(mktemp -d -t rmkit-poppler.XXXXXX)
trap 'rm -rf "$WORKDIR"' EXIT

PKG_XZ="$WORKDIR/Packages.xz"
PKG_TXT="$WORKDIR/Packages"
ROOT="$WORKDIR/root"
mkdir -p "$ROOT" "$OUT_DIR/bin" "$OUT_DIR/lib"

curl -fsSL "$MIRROR/dists/$SUITE/main/binary-$ARCH/Packages.xz" -o "$PKG_XZ"
python3 - "$PKG_XZ" "$PKG_TXT" "$WORKDIR/packages.txt" <<'PY'
import lzma
import re
import sys

packages_xz, packages_txt, selected_txt = sys.argv[1:]
data = lzma.open(packages_xz, "rt", encoding="utf-8", errors="replace").read()
open(packages_txt, "w", encoding="utf-8").write(data)

stanzas = {}
current = {}
last_key = None
for line in data.splitlines():
    if not line:
        if current.get("Package"):
            stanzas[current["Package"]] = current
        current = {}
        last_key = None
        continue
    if line.startswith(" ") and last_key:
        current[last_key] += "\n" + line
        continue
    key, value = line.split(":", 1)
    current[key] = value.strip()
    last_key = key
if current.get("Package"):
    stanzas[current["Package"]] = current

def dep_names(raw):
    names = []
    for item in re.split(r",\s*", raw or ""):
        first = item.split("|", 1)[0]
        first = re.sub(r"\s*\(.*?\)", "", first)
        first = re.sub(r":any\b|:native\b", "", first)
        first = first.strip()
        if first:
            names.append(first)
    return names

queue = ["poppler-utils"]
seen = set()
while queue:
    name = queue.pop(0)
    if name in seen or name not in stanzas:
        continue
    seen.add(name)
    stanza = stanzas[name]
    deps = dep_names(stanza.get("Pre-Depends", "")) + dep_names(stanza.get("Depends", ""))
    queue.extend(deps)

with open(selected_txt, "w", encoding="utf-8") as f:
    for name in sorted(seen):
        f.write(f"{name}\t{stanzas[name]['Filename']}\n")
PY

while IFS=$'\t' read -r package filename; do
  deb="$WORKDIR/${package}.deb"
  curl -fsSL "$MIRROR/$filename" -o "$deb"
  debdir="$WORKDIR/deb-$package"
  mkdir -p "$debdir"
  (cd "$debdir" && ar x "$deb")
  data_tar=$(find "$debdir" -maxdepth 1 -name 'data.tar.*' -print -quit)
  tar -xf "$data_tar" -C "$ROOT"
done < "$WORKDIR/packages.txt"

cp "$ROOT/usr/bin/pdftotext" "$OUT_DIR/bin/pdftotext.real"
chmod +x "$OUT_DIR/bin/pdftotext.real"

rm -f "$OUT_DIR/lib/"*
for libdir in "$ROOT/lib/aarch64-linux-gnu" "$ROOT/usr/lib/aarch64-linux-gnu"; do
  [ -d "$libdir" ] || continue
  find "$libdir" -maxdepth 1 \( -type f -o -type l \) -name '*.so*' | while read -r lib; do
    base=$(basename "$lib")
    case "$base" in
      ld-linux-aarch64.so.1|libc.so.*|libm.so.*|libpthread.so.*|libdl.so.*|librt.so.*|libresolv.so.*|libnsl.so.*|libutil.so.*|libgcc_s.so.*|libstdc++.so.*)
        continue
        ;;
    esac
    cp -L "$lib" "$OUT_DIR/lib/$base"
  done
done

{
  echo "source=$MIRROR $SUITE $ARCH"
  echo "packages:"
  cut -f1 "$WORKDIR/packages.txt" | sed 's/^/  /'
} > "$OUT_DIR/manifest.txt"

if command -v aarch64-linux-gnu-readelf >/dev/null 2>&1; then
  python3 - "$OUT_DIR" <<'PY'
import os
import re
import subprocess
import sys

out_dir = sys.argv[1]
bin_dir = os.path.join(out_dir, "bin")
lib_dir = os.path.join(out_dir, "lib")
paths = {}
for directory in (bin_dir, lib_dir):
    for name in os.listdir(directory):
        path = os.path.join(directory, name)
        if os.path.isfile(path):
            paths[name] = path

system = {
    "ld-linux-aarch64.so.1",
    "libc.so.6",
    "libdl.so.2",
    "libgcc_s.so.1",
    "libm.so.6",
    "libnsl.so.1",
    "libpthread.so.0",
    "libresolv.so.2",
    "librt.so.1",
    "libstdc++.so.6",
    "libutil.so.1",
}

needed = set()
queue = ["pdftotext.real"]
seen = set()
while queue:
    name = queue.pop(0)
    if name in seen:
        continue
    seen.add(name)
    path = paths.get(name)
    if not path:
        continue
    output = subprocess.run(
        ["aarch64-linux-gnu-readelf", "-d", path],
        text=True,
        capture_output=True,
        check=False,
    ).stdout
    for match in re.finditer(r"Shared library: \[(.*?)\]", output):
        dep = match.group(1)
        if dep in system:
            continue
        if dep not in needed:
            needed.add(dep)
            queue.append(dep)

for name in os.listdir(lib_dir):
    path = os.path.join(lib_dir, name)
    if os.path.isfile(path) and name not in needed:
        os.remove(path)
PY
fi

echo "Wrote $OUT_DIR"
