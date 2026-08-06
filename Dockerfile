# Image that contains the gha-docker-controller controller binary.
#
# The primary distribution path is native systemd
# (deploy/gha-docker-controller.service); this image is the auxiliary path.
#
# The controller holds control privileges over the Docker host (the Docker
# socket). These privileges are never inherited by runner containers. This
# image contains no Docker CLI, dockerd, GitHub runner or runner credential.
# The only thing passed to a runner is the JIT config; no setup mounts this
# image's config/secret or the Docker socket into a runner.
#
# The two bind mounts required at run time (docker run):
#   -v /var/run/docker.sock:/var/run/docker.sock
#       Docker daemon socket. The default host is unix:///var/run/docker.sock.
#   -v /etc/gha-docker-controller:/etc/gha-docker-controller:ro
#       Config and secret files (App private key, ...). The default path is
#       /etc/gha-docker-controller/config.yaml.
#   With PAT auth, pass the env var with -e GITHUB_TOKEN=...
#   (the PAT is not put in a file or in the YAML).
#
# Inside the container it runs as root and connects to the host's root:docker
# 0660 Docker socket.

# --- build stage ---
# The Go toolchain pins a version tag matching go 1.25.3 by a multi-arch
# index digest. latest and floating tags are not used. The release
# toolchain is limited to Go 1.25.3. go.mod has no toolchain directive, so
# this digest pin is the toolchain enforcement point of the container build;
# nothing other than go1.25.3 is used unless this changes.
FROM golang:1.25.3@sha256:6d4e5e74f47db00f7f24da5f53c1b4198ae46862a47395e30477365458347bf2 AS build

WORKDIR /src

# COPY go.mod/go.sum first so dependency resolution lands in the layer
# cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build info injection. The values are passed with --build-arg at build
# time and fall back to dev/unknown when unset (the internal/buildinfo
# contract).
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

# Build a static Linux binary. CGO_ENABLED=0 removes the libc dependency,
# so the binary can run in the scratch final stage. The CA bundle needed
# for TLS verification in the next stage must exist in the base image; it is
# verified at build time (the golang image ships ca-certificates from
# buildpack-deps).
RUN test -f /etc/ssl/certs/ca-certificates.crt \
    && CGO_ENABLED=0 go build -trimpath \
        -ldflags "-s -w \
            -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Version=${VERSION} \
            -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Commit=${COMMIT} \
            -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Date=${DATE}" \
        -o /out/gha-docker-controller ./cmd/gha-docker-controller

# --- final stage ---
# Minimal image with only the controller binary and the CA bundle. No
# toolchain, Docker CLI, runner or credential.
FROM scratch

COPY --from=build /out/gha-docker-controller /usr/local/bin/gha-docker-controller
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt

# Runs as root to access the Docker socket.
# Runner containers run as User=runner, so the controller privileges are not
# inherited.
ENTRYPOINT ["/usr/local/bin/gha-docker-controller", "serve"]
# State the default config path explicitly. Use --config to override it.
CMD ["--config", "/etc/gha-docker-controller/config.yaml"]
