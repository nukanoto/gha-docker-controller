# The controller image contains no runner, Docker CLI, daemon, or credentials.
# It still runs as root because it needs the mounted Docker socket.

# Pin the toolchain digest so builds do not silently change compiler versions.
FROM golang:1.25.3@sha256:6d4e5e74f47db00f7f24da5f53c1b4198ae46862a47395e30477365458347bf2 AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

# CGO is disabled for the scratch image; verify the CA bundle needed for TLS.
RUN test -f /etc/ssl/certs/ca-certificates.crt \
    && CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w \
            -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Version=${VERSION} \
            -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Commit=${COMMIT} \
            -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Date=${DATE}" \
        -o /out/gha-docker-controller ./cmd/gha-docker-controller

# Keep only the binary and CA bundle in the runtime image.
FROM scratch

COPY --from=build /out/gha-docker-controller /usr/local/bin/gha-docker-controller
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

ENTRYPOINT ["/usr/local/bin/gha-docker-controller", "serve"]
CMD ["--config", "/etc/gha-docker-controller/config.yaml"]
