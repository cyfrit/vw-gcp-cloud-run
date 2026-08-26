#!/usr/bin/env bash
set -euo pipefail

image="${LOCAL_IMAGE:-local/vw-gcp-cloud-run:verify}"

test -z "$(gofmt -l cmd/vw-gcp-cloud-run/main.go cmd/vw-gcp-cloud-run/main_test.go)"
git diff --check
go test -count=1 -race ./...
go vet ./...
npm exec --yes yaml-lint -- deploy/cloud-run.yaml litestream.yml

docker build --pull --tag "${image}" .
test "$(docker image inspect "${image}" --format '{{ index .Config.Labels "org.opencontainers.image.licenses" }}')" = 'MIT AND AGPL-3.0-only AND Apache-2.0'
docker run --rm --entrypoint /usr/local/bin/litestream \
  -e DATABASE_PATH=/tmp/db.sqlite3 \
  -e LITESTREAM_SOCKET=/tmp/litestream.sock \
  -e LITESTREAM_REPLICA_URL=file:///tmp/replica \
  "${image}" databases -config /etc/litestream.yml
docker run --rm --entrypoint /vaultwarden "${image}" --version
docker run --rm --entrypoint /usr/local/bin/litestream "${image}" version
docker run --rm --entrypoint /bin/sh "${image}" -c '
  test -x /usr/local/bin/vw-gcp-cloud-run &&
  test -x /usr/local/bin/litestream &&
  test -f /usr/share/licenses/vw-gcp-cloud-run/LICENSE &&
  test -f /usr/share/licenses/vaultwarden/LICENSE &&
  test -f /usr/share/licenses/litestream/LICENSE &&
  test -f /usr/share/doc/vw-gcp-cloud-run/THIRD_PARTY_NOTICES.md
'
