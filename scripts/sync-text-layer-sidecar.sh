#!/usr/bin/env bash
set -euo pipefail

DEVICE_TARGET="${DEVICE_TARGET:-remarkable}"

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <document-id>" >&2
  exit 2
fi

DOC_ID="$1"
case "$DOC_ID" in
  ""|*[!A-Za-z0-9-]*)
    echo "invalid document id: $DOC_ID" >&2
    exit 2
    ;;
esac
SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
WORKDIR=$(mktemp -d -t rmkit-text-layer.XXXXXX)
trap 'rm -rf "$WORKDIR"' EXIT

PDF_PATH="/home/root/.local/share/remarkable/xochitl/$DOC_ID.pdf"

scp "$DEVICE_TARGET:$PDF_PATH" "$WORKDIR/$DOC_ID.pdf"
PDFTOTEXT_BIN="${PDFTOTEXT_BIN:-pdftotext}"
(
  cd "$REPO_ROOT/upload-server-go"
  go run ./cmd/text-layer-sidecar \
    -pdf "$WORKDIR/$DOC_ID.pdf" \
    -output "$WORKDIR/$DOC_ID.json" \
    -pages-dir "$WORKDIR/$DOC_ID" \
    -pdftotext "$PDFTOTEXT_BIN"
)

ssh "$DEVICE_TARGET" 'doc=$1; rm -rf "/home/root/xovi/translate-text-layer/$doc"; mkdir -p /home/root/xovi/translate-text-layer' sh "$DOC_ID"
scp -r "$WORKDIR/$DOC_ID" "$DEVICE_TARGET:/home/root/xovi/translate-text-layer/"
scp "$WORKDIR/$DOC_ID.json" "$DEVICE_TARGET:/home/root/xovi/translate-text-layer/$DOC_ID.json"

ssh "$DEVICE_TARGET" 'doc=$1; find "/home/root/xovi/translate-text-layer/$doc" -maxdepth 1 -type f | wc -l' sh "$DOC_ID"
