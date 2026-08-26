#!/usr/bin/env bash
set -euo pipefail

for command in docker gh jq sed; do
  command -v "${command}" >/dev/null || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done

amd64_digest() {
  docker buildx imagetools inspect --raw "$1" | jq -er '
    .manifests[]
    | select(.platform.os == "linux" and .platform.architecture == "amd64")
    | .digest
  '
}

vaultwarden_version="$(gh api repos/dani-garcia/vaultwarden/releases/latest --jq .tag_name)"
litestream_version="$(gh api repos/benbjohnson/litestream/releases/latest --jq .tag_name)"

case "${vaultwarden_version}" in
  v*)
    echo "unexpected Vaultwarden release tag: ${vaultwarden_version}" >&2
    exit 1
    ;;
esac
case "${litestream_version}" in
  v*) ;;
  *)
    echo "unexpected Litestream release tag: ${litestream_version}" >&2
    exit 1
    ;;
esac

vaultwarden_image="docker.io/vaultwarden/server:${vaultwarden_version}"
litestream_image_tag="${litestream_version#v}"
litestream_image="docker.io/litestream/litestream:${litestream_image_tag}"
vaultwarden_digest="$(amd64_digest "${vaultwarden_image}")"
litestream_digest="$(amd64_digest "${litestream_image}")"

sed -i \
  -e "s|^ARG VAULTWARDEN_VERSION=.*|ARG VAULTWARDEN_VERSION=${vaultwarden_version}|" \
  -e "s|^ARG VAULTWARDEN_IMAGE=.*|ARG VAULTWARDEN_IMAGE=${vaultwarden_image}@${vaultwarden_digest}|" \
  -e "s|^ARG LITESTREAM_VERSION=.*|ARG LITESTREAM_VERSION=${litestream_version}|" \
  -e "s|^ARG LITESTREAM_IMAGE=.*|ARG LITESTREAM_IMAGE=${litestream_image}@${litestream_digest}|" \
  Dockerfile

sed -i \
  -e "s#^| Vaultwarden server |.*#| Vaultwarden server | \`${vaultwarden_version}\`, \`${vaultwarden_image}@${vaultwarden_digest}\` | AGPL-3.0-only | https://github.com/dani-garcia/vaultwarden/tree/${vaultwarden_version} | [\`licenses/VAULTWARDEN_AGPL-3.0.txt\`](licenses/VAULTWARDEN_AGPL-3.0.txt) |#" \
  -e "s#^| Litestream |.*#| Litestream | \`${litestream_version}\`, \`${litestream_image}@${litestream_digest}\` | Apache-2.0 | https://github.com/benbjohnson/litestream/tree/${litestream_version} | [\`licenses/LITESTREAM_LICENSE\`](licenses/LITESTREAM_LICENSE) |#" \
  THIRD_PARTY_NOTICES.md

if git diff --quiet -- Dockerfile THIRD_PARTY_NOTICES.md; then
  echo "upstream images are already current"
else
  echo "updated Vaultwarden ${vaultwarden_version} and Litestream ${litestream_version}"
fi
