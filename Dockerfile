# Build the binary. No module downloads happen here: the project deliberately
# has no third-party dependencies, so this stage works offline.
FROM golang:1.24-alpine AS build

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w \
        -X github.com/karba-studio/outline-backup/internal/version.Version=${VERSION} \
        -X github.com/karba-studio/outline-backup/internal/version.Commit=${COMMIT} \
        -X github.com/karba-studio/outline-backup/internal/version.Date=${DATE}" \
      -o /out/outline-backup ./cmd/outline-backup

# restic does the encryption, deduplication and retention.
FROM restic/restic:latest AS restic

# The docker CLI: this tool dumps Postgres through the running container rather
# than over TCP, so it never needs a pg_dump whose version matches the server.
FROM docker:cli AS dockercli

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata && \
    adduser -D -u 10001 omb

COPY --from=build     /out/outline-backup /usr/local/bin/outline-backup
COPY --from=restic    /usr/bin/restic           /usr/local/bin/restic
COPY --from=dockercli /usr/local/bin/docker     /usr/local/bin/docker

# Config and secrets are mounted in; staging is scratch space for one run.
ENV OMB_CONFIG=/config/config.json
VOLUME ["/config", "/staging"]

# NOTE: reaching the stack requires the Docker socket, which is equivalent to
# root on the host. On a machine where you already run the stack yourself, the
# native binary is the smaller blast radius. See docs/INSTALL.md.
USER root

ENTRYPOINT ["/usr/local/bin/outline-backup"]
CMD ["agent"]
