ARG VAULTWARDEN_VERSION=1.37.2
ARG VAULTWARDEN_IMAGE=docker.io/vaultwarden/server:1.37.2@sha256:5d326778c22f063d093d6b0c9c766a28249561632266776f2c93132ab0ad3a80
ARG LITESTREAM_VERSION=v0.5.16
ARG LITESTREAM_IMAGE=docker.io/litestream/litestream:0.5.16@sha256:837279d1279a90d670c915ddbb641b6e7b98bc42cc4dfb040eb569ee931078a1

FROM golang:1.25-bookworm AS supervisor-build
WORKDIR /src
COPY go.mod ./
COPY cmd/vw-gcp-cloud-run ./cmd/vw-gcp-cloud-run
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/vw-gcp-cloud-run ./cmd/vw-gcp-cloud-run

FROM ${LITESTREAM_IMAGE} AS litestream

FROM ${VAULTWARDEN_IMAGE}
ARG VAULTWARDEN_VERSION
ARG LITESTREAM_VERSION
LABEL org.opencontainers.image.source="https://github.com/cyfrit/vw-gcp-cloud-run" \
      org.opencontainers.image.licenses="MIT AND AGPL-3.0-only AND Apache-2.0" \
      io.github.cyfrit.vw-gcp-cloud-run.vaultwarden.version="${VAULTWARDEN_VERSION}" \
      io.github.cyfrit.vw-gcp-cloud-run.vaultwarden.source="https://github.com/dani-garcia/vaultwarden/tree/${VAULTWARDEN_VERSION}" \
      io.github.cyfrit.vw-gcp-cloud-run.litestream.version="${LITESTREAM_VERSION}" \
      io.github.cyfrit.vw-gcp-cloud-run.litestream.source="https://github.com/benbjohnson/litestream/tree/${LITESTREAM_VERSION}"
COPY --from=supervisor-build /out/vw-gcp-cloud-run /usr/local/bin/vw-gcp-cloud-run
COPY --from=litestream /usr/local/bin/litestream /usr/local/bin/litestream
COPY litestream.yml /etc/litestream.yml
COPY LICENSE /usr/share/licenses/vw-gcp-cloud-run/LICENSE
COPY licenses/VAULTWARDEN_AGPL-3.0.txt /usr/share/licenses/vaultwarden/LICENSE
COPY licenses/LITESTREAM_LICENSE /usr/share/licenses/litestream/LICENSE
COPY THIRD_PARTY_NOTICES.md /usr/share/doc/vw-gcp-cloud-run/THIRD_PARTY_NOTICES.md

HEALTHCHECK NONE
ENTRYPOINT ["/usr/local/bin/vw-gcp-cloud-run"]
CMD []
