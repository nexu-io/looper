#!/bin/sh
# Publish the generated release manifest to Cloudflare R2.
#
# Layout (bucket looper-releases, public host releases.looper.powerformer.com):
#   <tag>/manifest.json       immutable per-release copy
#   channels/<channel>.json   mutable latest pointer for that channel
#   manifest.json             alias of the latest stable pointer
#
# Required: wrangler (or npx wrangler) authenticated to the Powerformer
# Cloudflare account. CI supplies CLOUDFLARE_API_TOKEN + CLOUDFLARE_ACCOUNT_ID.

set -eu

BUCKET="${R2_RELEASES_BUCKET:-looper-releases}"
MANIFEST="${RELEASE_MANIFEST:-release-assets/manifest.json}"
CONTENT_TYPE="application/json; charset=utf-8"
VERSIONED_CACHE="public, max-age=31536000, immutable"
POINTER_CACHE="public, max-age=60"

if [ ! -f "$MANIFEST" ]; then
  echo "release manifest not found: $MANIFEST" >&2
  exit 1
fi

read_manifest_field() {
  python3 -c 'import json, sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' "$MANIFEST" "$1"
}

TAG="${RELEASE_TAG:-$(read_manifest_field tag)}"
CHANNEL="${RELEASE_CHANNEL:-$(read_manifest_field channel)}"

if [ -z "$TAG" ] || [ -z "$CHANNEL" ]; then
  echo "manifest is missing tag or channel" >&2
  exit 1
fi

wrangler_bin() {
  if command -v wrangler >/dev/null 2>&1; then
    wrangler "$@"
    return
  fi
  npx --yes wrangler@4 "$@"
}

put_object() {
  key="$1"
  cache="$2"
  echo "uploading $MANIFEST -> r2://${BUCKET}/${key}"
  wrangler_bin r2 object put "${BUCKET}/${key}" \
    --file "$MANIFEST" \
    --content-type "$CONTENT_TYPE" \
    --cache-control "$cache" \
    --remote
}

put_object "${TAG}/manifest.json" "$VERSIONED_CACHE"
put_object "channels/${CHANNEL}.json" "$POINTER_CACHE"

if [ "$CHANNEL" = "stable" ]; then
  put_object "manifest.json" "$POINTER_CACHE"
fi
