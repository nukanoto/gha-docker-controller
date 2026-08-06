package config

// This file verifies the parse functions (Duration, Memory, NanoCPUs,
// Ulimit) and format pure functions with value inputs. No temporary files or
// external I/O are used; the path where yaml.v3 stringifies int/float scalars
// is covered too.

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// unmarshalInto YAML-unmarshals doc into v, verifies wantErr and returns the
// error.
func unmarshalInto(t *testing.T, name string, doc string, v any, wantErr string) error {
	t.Helper()
	err := yaml.Unmarshal([]byte(doc), v)
	checkErr(t, name, wantErr, err)
	return err
}

// TestParseDuration_UnitRequiredAndPositive accepts only strings with a unit
// and rejects bare numbers and non-positive values.
func TestParseDuration_UnitRequiredAndPositive(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    time.Duration
		wantErr string
	}{
		{name: "valid minutes", doc: "5m", want: 5 * time.Minute},
		{name: "valid seconds", doc: "30s", want: 30 * time.Second},
		{name: "valid compound", doc: "1h30m", want: 90 * time.Minute},
		{name: "bare integer is rejected", doc: "300", wantErr: "not a bare number"},
		{name: "bare float is rejected", doc: "0.5", wantErr: "not a bare number"},
		{name: "zero is rejected", doc: "0s", wantErr: "duration must be positive"},
		{name: "negative is rejected", doc: "-5m", wantErr: "duration must be positive"},
		{name: "garbage is rejected", doc: "abc", wantErr: "invalid duration"},
		{name: "empty string is rejected", doc: `""`, wantErr: "invalid duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Duration
			if err := unmarshalInto(t, tt.name, tt.doc, &got, tt.wantErr); err != nil || tt.wantErr != "" {
				return
			}
			if got != Duration(tt.want) {
				t.Fatalf("parse 結果が不正です: 期待値 %v、実測値 %v", tt.want, got)
			}
		})
	}
}

// TestParseMemory_UnitsAndUnlimited verifies the same powers-of-1024 units as
// Docker units.RAMInBytes and the rejection of unlimited values. All units
// such as k/kb/kib and m/mb/mib are powers of 1024.
func TestParseMemory_UnitsAndUnlimited(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    int64
		wantErr string
	}{
		{name: "bare bytes", in: "4096", want: 4096},
		{name: "k is 1024", in: "1k", want: 1024},
		{name: "kb is 1024", in: "1kb", want: 1024},
		{name: "kib is 1024", in: "1kib", want: 1024},
		{name: "m is 1024^2", in: "1m", want: 1024 * 1024},
		{name: "mb is 1024^2", in: "1mb", want: 1024 * 1024},
		{name: "g is 1024^3", in: "1g", want: 1024 * 1024 * 1024},
		{name: "gb is 1024^3", in: "1gb", want: 1024 * 1024 * 1024},
		{name: "gib is 1024^3", in: "1GiB", want: 1024 * 1024 * 1024},
		{name: "t is 1024^4", in: "1t", want: 1 << 40},
		{name: "tb is 1024^4", in: "1tb", want: 1 << 40},
		{name: "fractional with unit", in: "1.5g", want: int64(1.5 * 1024 * 1024 * 1024)},
		{name: "zero is unlimited and rejected", in: "0", wantErr: "unlimited memory value"},
		{name: "minus one is unlimited and rejected", in: "-1", wantErr: "unlimited memory value"},
		{name: "unlimited keyword rejected", in: "unlimited", wantErr: "unlimited memory value"},
		{name: "unset keyword rejected", in: "unset", wantErr: "unlimited memory value"},
		{name: "none keyword rejected", in: "none", wantErr: "unlimited memory value"},
		{name: "negative is rejected", in: "-512MiB", wantErr: "invalid memory value"},
		{name: "fractional without unit is rejected", in: "1.5", wantErr: "integer number of bytes"},
		{name: "empty is rejected", in: "", wantErr: "must not be empty"},
		{name: "unknown unit is rejected", in: "1zz", wantErr: "invalid memory value"},
		{name: "no digits are rejected", in: "g", wantErr: "invalid memory value"},
		{name: "overflow is rejected", in: "100000000t", wantErr: "too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMemory(tt.in)
			checkErr(t, tt.name, tt.wantErr, err)
			if tt.wantErr == "" && got != tt.want {
				t.Fatalf("parse 結果が不正です: 期待値 %d、実測値 %d", tt.want, got)
			}
		})
	}
}

// TestMemoryUnmarshal_YAMLScalarForms verifies that Memory accepts both YAML
// numeric scalars and strings with a unit.
func TestMemoryUnmarshal_YAMLScalarForms(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    Memory
		wantErr string
	}{
		{name: "bare integer bytes", doc: "4096", want: Memory(4096)},
		{name: "gib string", doc: `"4GiB"`, want: Memory(4 << 30)},
		{name: "mb string", doc: `"1mb"`, want: Memory(1024 * 1024)},
		{name: "zero is rejected", doc: "0", wantErr: "unlimited memory value"},
		{name: "minus one is rejected", doc: "-1", wantErr: "unlimited memory value"},
		{name: "fractional bytes are rejected", doc: "1.5", wantErr: "integer number of bytes"},
		{name: "negative string is rejected", doc: `"-512MiB"`, wantErr: "invalid memory value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Memory
			if err := unmarshalInto(t, tt.name, tt.doc, &got, tt.wantErr); err != nil || tt.wantErr != "" {
				return
			}
			if got != tt.want {
				t.Fatalf("parse 結果が不正です: 期待値 %d、実測値 %d", tt.want, got)
			}
		})
	}
}

// TestParseCPU_PositiveOnly verifies that CPU accepts only values greater
// than 0 and rejects unlimited and oversized values.
func TestParseCPU_PositiveOnly(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    NanoCPUs
		wantErr string
	}{
		{name: "integer cpu", doc: "2", want: NanoCPUs(2000000000)},
		{name: "decimal cpu", doc: "2.5", want: NanoCPUs(2500000000)},
		{name: "quoted string cpu", doc: `"1.5"`, want: NanoCPUs(1500000000)},
		{name: "zero is rejected", doc: "0", wantErr: "cpu must be positive"},
		{name: "negative is rejected", doc: "-1", wantErr: "cpu must be positive"},
		{name: "unlimited is rejected", doc: "unlimited", wantErr: "invalid cpu value"},
		{name: "unset is rejected", doc: "unset", wantErr: "invalid cpu value"},
		{name: "too large is rejected", doc: "1e7", wantErr: "cpu value is too large"},
		{name: "garbage is rejected", doc: "abc", wantErr: "invalid cpu value"},
		{name: "empty is rejected", doc: `""`, wantErr: "invalid cpu value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got NanoCPUs
			if err := unmarshalInto(t, tt.name, tt.doc, &got, tt.wantErr); err != nil || tt.wantErr != "" {
				return
			}
			if got != tt.want {
				t.Fatalf("parse 結果が不正です: 期待値 %d、実測値 %d", tt.want, got)
			}
		})
	}
}

// TestParseUlimit_NameSoftHard verifies that ulimit accepts only
// name=soft[:hard] form with positive integer soft/hard values.
func TestParseUlimit_NameSoftHard(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    Ulimit
		wantErr string
	}{
		{name: "soft only", doc: "nofile=1024", want: Ulimit{Name: "nofile", Soft: 1024, Hard: 1024}},
		{name: "soft and hard", doc: "nofile=1024:2048", want: Ulimit{Name: "nofile", Soft: 1024, Hard: 2048}},
		{name: "zero soft is rejected", doc: "nofile=0", wantErr: "invalid ulimit"},
		{name: "zero hard is rejected", doc: "nofile=1024:0", wantErr: "invalid ulimit"},
		{name: "negative soft is rejected", doc: "nofile=-1", wantErr: "invalid ulimit"},
		{name: "negative hard is rejected", doc: "nofile=1024:-1", wantErr: "invalid ulimit"},
		{name: "hard below soft is rejected", doc: "nofile=2048:1024", wantErr: "invalid ulimit"},
		{name: "unknown resource name is rejected", doc: "bogus=1024", wantErr: "invalid ulimit"},
		{name: "missing name is rejected", doc: "=1024", wantErr: "invalid ulimit"},
		{name: "missing equals is rejected", doc: "nofile", wantErr: "invalid ulimit"},
		{name: "invalid name is rejected", doc: "no file=1", wantErr: "invalid ulimit"},
		{name: "extra colon is rejected", doc: "nofile=1:2:3", wantErr: "invalid ulimit"},
		{name: "empty is rejected", doc: `""`, wantErr: "invalid ulimit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got Ulimit
			if err := unmarshalInto(t, tt.name, tt.doc, &got, tt.wantErr); err != nil || tt.wantErr != "" {
				return
			}
			if got != tt.want {
				t.Fatalf("parse 結果が不正です: 期待値 %+v、実測値 %+v", tt.want, got)
			}
		})
	}
}

// TestNormalizeDockerHost_TrimsTrailingSlash verifies the normalization that
// removes the trailing "/" of a unix socket path.
func TestNormalizeDockerHost_TrimsTrailingSlash(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default socket is unchanged", in: "unix:///var/run/docker.sock", want: "unix:///var/run/docker.sock"},
		{name: "trailing slash is trimmed", in: "unix:///var/run/docker.sock/", want: "unix:///var/run/docker.sock"},
		{name: "multiple trailing slashes are trimmed", in: "unix:///tmp//", want: "unix:///tmp"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeDockerHost(tt.in); got != tt.want {
				t.Fatalf("正規化結果が不正です: 期待値 %q、実測値 %q", tt.want, got)
			}
		})
	}
}

// TestValidName_AllowedCharacters verifies the [A-Za-z0-9_.-]+ character set
// of identifiers embedded directly into queries.
func TestValidName_AllowedCharacters(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "alphanumeric with separators", in: "my-org_1.prod", want: true},
		{name: "single character", in: "a", want: true},
		{name: "empty is invalid", in: "", want: false},
		{name: "space is invalid", in: "a b", want: false},
		{name: "slash is invalid", in: "a/b", want: false},
		{name: "colon is invalid", in: "a:b", want: false},
		{name: "japanese is invalid", in: "日本語", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := validName(tt.in); got != tt.want {
				t.Fatalf("validName(%q) が不正です: 期待値 %v、実測値 %v", tt.in, tt.want, got)
			}
		})
	}
}

// TestValidateGitHubURL_GitHubComOnly verifies that github.url is exactly
// https://github.com. GHES, ghe.com, github.localhost and
// port/query/fragment/path/userinfo are rejected.
func TestValidateGitHubURL_GitHubComOnly(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr string
	}{
		{name: "exact url is allowed", url: "https://github.com"},
		{name: "root path is allowed", url: "https://github.com/"},
		{name: "http scheme is rejected", url: "http://github.com", wantErr: "scheme must be https"},
		{name: "ghe.com is rejected", url: "https://ghe.com", wantErr: "host must be exactly github.com"},
		{name: "github.localhost is rejected", url: "https://github.localhost", wantErr: "host must be exactly github.com"},
		{name: "lookalike domain is rejected", url: "https://github.com.evil.example", wantErr: "host must be exactly github.com"},
		{name: "port is rejected", url: "https://github.com:443", wantErr: "port is not allowed"},
		{name: "userinfo is rejected", url: "https://user@github.com", wantErr: "userinfo is not allowed"},
		{name: "path is rejected", url: "https://github.com/actions", wantErr: "path is not allowed"},
		{name: "query is rejected", url: "https://github.com/?a=b", wantErr: "query is not allowed"},
		{name: "fragment is rejected", url: "https://github.com/#frag", wantErr: "fragment is not allowed"},
		{name: "empty is rejected", url: "", wantErr: "required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.name, tt.wantErr, validateGitHubURL(tt.url))
		})
	}
}

// TestValidateDockerHost_UnixSocketOnly verifies that docker.host allows only
// an absolute-path unix://.
func TestValidateDockerHost_UnixSocketOnly(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		wantErr string
	}{
		{name: "default socket is allowed", host: "unix:///var/run/docker.sock"},
		{name: "custom socket is allowed", host: "unix:///tmp/ghadc.sock"},
		{name: "tcp is rejected", host: "tcp://127.0.0.1:2375", wantErr: "only unix://"},
		{name: "ssh is rejected", host: "ssh://user@host", wantErr: "only unix://"},
		{name: "relative path is rejected", host: "unix://var/run/docker.sock", wantErr: "absolute"},
		{name: "root path is rejected", host: "unix:///", wantErr: "must not be or end with '/'"},
		{name: "trailing slash is rejected", host: "unix:///var/run/", wantErr: "must not be or end with '/'"},
		{name: "empty is rejected", host: "", wantErr: "required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.name, tt.wantErr, validateDockerHost(tt.host))
		})
	}
}

// TestValidateNetwork_HostNoneAndContainerRejected verifies that host, none
// and container modes are rejected and only bridge or user-defined networks
// are allowed.
func TestValidateNetwork_HostNoneAndContainerRejected(t *testing.T) {
	tests := []struct {
		name    string
		network string
		wantErr string
	}{
		{name: "bridge is allowed", network: "bridge"},
		{name: "user-defined network is allowed", network: "my-net"},
		{name: "network named container prefix is allowed", network: "container-net"},
		{name: "host network is rejected", network: "host", wantErr: "host network is not allowed"},
		{name: "none is rejected", network: "none", wantErr: "not allowed"},
		{name: "container mode is rejected", network: "container", wantErr: "network mode \"container\" is not allowed"},
		{name: "container:id mode is rejected", network: "container:db", wantErr: "network mode \"container:db\" is not allowed"},
		{name: "container:name mode is rejected", network: "container:my-service", wantErr: "network mode \"container:my-service\" is not allowed"},
		{name: "empty is rejected", network: "", wantErr: "required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.name, tt.wantErr, validateNetwork(tt.network))
		})
	}
}

// TestValidateImage_LatestTaglessAndDigest verifies that latest and
// tagless/digestless references are rejected and dind-runner allows only
// digest references. latest is explicitly rejected both as a tag and with a
// digest.
func TestValidateImage_LatestTaglessAndDigest(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		profile string
		wantErr string
	}{
		{name: "version tag is allowed", image: "ghcr.io/actions/actions-runner:2.336.0", profile: ProfileStandard},
		{name: "sha256 digest is allowed", image: "ghcr.io/actions/actions-runner@" + sha256Digest, profile: ProfileStandard},
		{name: "sha512 digest is allowed", image: "ghcr.io/actions/actions-runner@sha512:" + strings.Repeat("a", 128), profile: ProfileStandard},
		{name: "dind digest is allowed", image: "ghcr.io/actions/actions-runner@" + sha256Digest, profile: ProfileDindRunner},
		{name: "latest tag is rejected", image: "ghcr.io/actions/actions-runner:latest", wantErr: "not allowed"},
		{name: "latest with digest is rejected", image: "ghcr.io/actions/actions-runner:latest@" + sha256Digest, wantErr: "not allowed"},
		{name: "bare latest with digest is rejected", image: "latest@" + sha256Digest, wantErr: "not allowed"},
		{name: "tagless is rejected", image: "ghcr.io/actions/actions-runner", wantErr: "tag or digest is required"},
		{name: "empty tag is rejected", image: "ghcr.io/actions/actions-runner:", wantErr: "invalid image name"},
		{name: "invalid tag is rejected", image: "ghcr.io/actions/actions-runner:2.336.0!", wantErr: "invalid image name"},
		{name: "empty image is rejected", image: "", wantErr: "required"},
		{name: "dind requires digest", image: "ghcr.io/actions/actions-runner:2.336.0", profile: ProfileDindRunner, wantErr: "requires a digest reference"},
		{name: "malformed digest is rejected", image: "ghcr.io/actions/actions-runner@sha256:short", wantErr: "invalid digest"},
		{name: "unknown digest algorithm is rejected", image: "ghcr.io/actions/actions-runner@md5:abc", wantErr: "invalid digest"},
		{name: "uppercase repository is rejected", image: "GHCR.IO/actions/actions-runner:2.336.0", wantErr: "invalid image name"},
		{name: "empty repository is rejected", image: "@sha256:" + strings.Repeat("a", 64), wantErr: "invalid image name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile := tt.profile
			if profile == "" {
				profile = ProfileStandard
			}
			checkErr(t, tt.name, tt.wantErr, validateImage(tt.image, profile))
		})
	}
}

// TestValidateTmpfs_DockerCLICompat verifies both dest:options and
// dest:size:options forms, using the presence of a comma to avoid mistaking
// options for a size.
func TestValidateTmpfs_DockerCLICompat(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{name: "destination only", spec: "/run"},
		{name: "dest:size form", spec: "/run:64MiB"},
		{name: "dest:size:options form", spec: "/run:64MiB:ro"},
		{name: "dest:options form", spec: "/run:ro"},
		{name: "size option inside comma options", spec: "/run:ro,size=64MiB"},
		{name: "size option alone", spec: "/run:size=64MiB"},
		{name: "options with multiple flags", spec: "/run:64MiB:rw,nosuid"},
		{name: "relative destination is rejected", spec: "run", wantErr: "absolute path"},
		{name: "empty spec is rejected", spec: "", wantErr: "must not be empty"},
		{name: "too many parts are rejected", spec: "/run:64MiB:ro:extra", wantErr: "invalid tmpfs spec"},
		{name: "size after options is rejected", spec: "/run:size=64MiB:ro", wantErr: "size must come before options"},
		{name: "unlimited size option is rejected", spec: "/run:size=0", wantErr: "invalid tmpfs size option"},
		{name: "empty destination is rejected", spec: ":64MiB", wantErr: "absolute path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.name, tt.wantErr, validateTmpfs(tt.spec))
		})
	}
}

// TestValidateExtraHost_HostIPOrHostGateway verifies that extraHosts allows
// only host:ip or host:host-gateway.
func TestValidateExtraHost_HostIPOrHostGateway(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantErr string
	}{
		{name: "ipv4 is allowed", spec: "host.internal:192.168.0.10"},
		{name: "ipv6 is allowed", spec: "host.internal:2001:db8::1"},
		{name: "host-gateway is allowed", spec: "host:host-gateway"},
		{name: "empty ip is rejected", spec: "host:", wantErr: "invalid IP address"},
		{name: "empty host is rejected", spec: ":1.1.1.1", wantErr: "expected host:ip"},
		{name: "missing colon is rejected", spec: "no-colon", wantErr: "expected host:ip"},
		{name: "invalid hostname is rejected", spec: "bad host:1.1.1.1", wantErr: "invalid hostname"},
		{name: "invalid ip is rejected", spec: "host:999.1.1.1", wantErr: "invalid IP address"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.name, tt.wantErr, validateExtraHost(tt.spec))
		})
	}
}

// TestValidateListen_HostPort verifies that the health listen address is in
// host:port form with the port in the 1..65535 range.
func TestValidateListen_HostPort(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		wantErr string
	}{
		{name: "ipv4 with port is allowed", addr: "127.0.0.1:8080"},
		{name: "ipv6 with port is allowed", addr: "[::1]:8080"},
		{name: "bare port is rejected", addr: "8080", wantErr: "must be host:port"},
		{name: "hostname without port is rejected", addr: "localhost", wantErr: "must be host:port"},
		{name: "port zero is rejected", addr: "127.0.0.1:0", wantErr: "invalid port"},
		{name: "port overflow is rejected", addr: "127.0.0.1:65536", wantErr: "invalid port"},
		{name: "negative port is rejected", addr: "127.0.0.1:-1", wantErr: "invalid port"},
		{name: "empty is rejected", addr: "", wantErr: "required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.name, tt.wantErr, validateListen(tt.addr))
		})
	}
}
