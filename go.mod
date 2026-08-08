module github.com/nukanoto/arc-docker

// No toolchain directive: Go 1.25.3's go mod tidy removes "go1.25.3" as
// redundant, so a directive would break the tidy -diff check. The release
// toolchain is limited to Go 1.25.3 by the README build command's version
// check and the Dockerfile base image digest pin.
go 1.25.3

// Only researched stable versions are pinned as direct dependencies;
// latest and floating tags are not used.
require (
	github.com/actions/scaleset v0.4.0
	github.com/containerd/errdefs v1.0.0
	github.com/hashicorp/go-retryablehttp v0.7.8 // indirect
	github.com/moby/moby/api v1.55.0
	github.com/moby/moby/client v0.5.1
	gopkg.in/yaml.v3 v3.0.1
)

// The transitive dependencies were derived by go mod tidy from the
// dependency graphs of moby client v0.5.1 and actions/scaleset v0.4.0;
// do not remove them by hand.
require (
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/distribution/reference v0.6.0
	github.com/docker/go-connections v0.7.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.60.0 // indirect
	go.opentelemetry.io/otel v1.39.0 // indirect
	go.opentelemetry.io/otel/metric v1.39.0 // indirect
	go.opentelemetry.io/otel/trace v1.39.0 // indirect
)

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/golang-jwt/jwt/v4 v4.5.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	golang.org/x/sys v0.42.0 // indirect
)
