package config

import (
	"slices"
	"time"
)

// Defaults are fixed across this package. maxRunners, image, CPU, memory,
// memorySwap and pidsLimit are mandatory and have no default (validate rejects
// them when missing).

const (
	// DefaultGitHubURL is the default GitHub.com base URL.
	DefaultGitHubURL = "https://github.com"
	// DefaultRunnerGroup is the default runner group name.
	DefaultRunnerGroup = "default"
	// DefaultDockerHost is the default Docker daemon unix socket path.
	DefaultDockerHost = "unix:///var/run/docker.sock"
	// DefaultRuntime is the default OCI runtime name.
	DefaultRuntime = "runsc"
	// DefaultNetwork is the default Docker network name.
	DefaultNetwork = "bridge"
	// DefaultPullPolicy is the default image pull policy.
	DefaultPullPolicy = "if-not-present"
	// DefaultProfile is the default runner profile.
	DefaultProfile = "standard"
	// DefaultHealthListen is the default listen address of the health endpoint.
	DefaultHealthListen = "127.0.0.1:8080"
	// DefaultShutdownPolicy is the default policy for Busy runners at shutdown.
	DefaultShutdownPolicy = "leave"
	// DefaultLogFormat is the default log format.
	DefaultLogFormat = "json"
	// DefaultLogLevel is the default log level.
	DefaultLogLevel = "info"
)

const (
	// DefaultProvisioningTimeout is the default deadline for provisioning.
	DefaultProvisioningTimeout = 5 * time.Minute
	// DefaultStopTimeout is the default grace period when stopping.
	DefaultStopTimeout = 30 * time.Second
	// DefaultShutdownGrace is the default wait time for Busy runners at shutdown.
	DefaultShutdownGrace = 2 * time.Minute
)

// DefaultDindStorage is the default storage kind for /var/lib/docker.
// Only tmpfs is allowed and the default is tmpfs as well.
const DefaultDindStorage = "tmpfs"

// DefaultDindStorageSize is the default tmpfs size of 2 GiB for /var/lib/docker.
const DefaultDindStorageSize = Memory(2 * 1024 * 1024 * 1024)

// dindCapAdd is the complete set of capabilities allowed for the
// dind-runner profile. Only the 17 capabilities needed to run the inner
// dockerd inside runsc are allowed; nothing else can be added. It is kept
// unexported and immutable, and DindCapabilities returns a defensive copy.
var dindCapAdd = []string{
	"AUDIT_WRITE",
	"CHOWN",
	"DAC_OVERRIDE",
	"FOWNER",
	"FSETID",
	"KILL",
	"MKNOD",
	"NET_BIND_SERVICE",
	"NET_ADMIN",
	"NET_RAW",
	"SETFCAP",
	"SETGID",
	"SETPCAP",
	"SETUID",
	"SYS_ADMIN",
	"SYS_CHROOT",
	"SYS_PTRACE",
}

// DindCapabilities returns a defensive copy of the capabilities allowed for
// the dind-runner profile. Mutating the returned slice does not affect the
// fixed set inside the package or future results.
func DindCapabilities() []string {
	return slices.Clone(dindCapAdd)
}
