# Builds the same reproducible binary as `make build` (see the Makefile),
# then runs it as an unprivileged, read-only container. See docs/configuration.md
# for SCOTMESH_CHAT_* environment variables and deploy/docker-compose.example.yml.

FROM golang:1.26.5 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
# Cache module downloads across builds, invalidated only when go.mod/go.sum
# or the vendored third_party module change.
COPY go.mod go.sum ./
COPY third_party/reticulum-go/go.mod third_party/reticulum-go/go.sum third_party/reticulum-go/
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -buildid= -X main.version=$VERSION" \
    -o /out/scotmesh-chat ./cmd/scotmesh-chat
# distroless has no shell to chown a volume with, so /data is prepared here,
# owned by distroless's nonroot uid:gid (65532:65532), and copied in below.
# Docker seeds a fresh named volume from the image directory it overlays,
# ownership included, so a first-run volume comes up writable by nonroot.
RUN mkdir -p /data && chown 65532:65532 /data

# distroless: no shell, no package manager — nothing to exploit if a bug
# reaches the container. :nonroot runs as an unprivileged uid.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/scotmesh-chat /scotmesh-chat
COPY --from=build --chown=65532:65532 /data /data
ENV SCOTMESH_CHAT_DATA_DIR=/data
VOLUME /data
# See docs/configuration.md — every setting can be passed as an env var, so
# no config file is required. --max-age matches the default 30s write
# interval with headroom.
HEALTHCHECK --interval=60s --timeout=5s --start-period=30s \
    CMD ["/scotmesh-chat", "healthcheck", "--max-age=90s"]
ENTRYPOINT ["/scotmesh-chat"]
