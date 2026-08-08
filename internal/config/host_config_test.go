package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
)

func hostConfigDoc(t *testing.T, body string) string {
	t.Helper()
	if body == "" {
		body = "    {}\n"
	}
	base := baseWithKey(t)
	return strings.Replace(base,
		"  image: ghcr.io/actions/actions-runner:2.336.0\n",
		"  image: ghcr.io/actions/actions-runner:2.336.0\n  hostConfig:\n"+body,
		1)
}

func TestLoad_HostConfigCaseInsensitiveFieldNames(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"lower camel", "capAdd"},
		{"Pascal case", "CapAdd"},
		{"upper case", "CAPADD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := loadDoc(t, hostConfigDoc(t, "    "+tt.key+": [CHOWN]\n"))
			if !reflect.DeepEqual(c.Runner.HostConfig.CapAdd, []string{"CHOWN"}) {
				t.Fatalf("CapAdd の変換結果が不正です: %v", c.Runner.HostConfig.CapAdd)
			}
		})
	}
}

func TestLoad_HostConfigFieldsAndNestedValues(t *testing.T) {
	doc := hostConfigDoc(t, `    DNS: [1.1.1.1]
    CPUQuota: 100000
    restartPolicy:
      Name: always
      MaximumRetryCount: 3
    mounts:
      - Type: tmpfs
        Target: /tmp/cache
        ReadOnly: true
    tmpfs:
      /Case/Path: rw
    sysctls:
      Net.IPv4.Foo: "1"
    privileged: true
    binds:
      - /host:/container
    runtime: custom-runtime
`)
	c, _ := loadDoc(t, doc)
	hc := c.Runner.HostConfig
	if len(hc.DNS) != 1 || hc.DNS[0].String() != "1.1.1.1" {
		t.Fatalf("DNS の変換結果が不正です: %v", hc.DNS)
	}
	if hc.CPUQuota != 100000 {
		t.Fatalf("CPUQuota の変換結果が不正です: %d", hc.CPUQuota)
	}
	if hc.RestartPolicy.Name != container.RestartPolicyAlways || hc.RestartPolicy.MaximumRetryCount != 3 {
		t.Fatalf("RestartPolicy の変換結果が不正です: %+v", hc.RestartPolicy)
	}
	if len(hc.Mounts) != 1 || hc.Mounts[0].Target != "/tmp/cache" || !hc.Mounts[0].ReadOnly {
		t.Fatalf("Mounts の変換結果が不正です: %+v", hc.Mounts)
	}
	if hc.Tmpfs["/Case/Path"] != "rw" || hc.Sysctls["Net.IPv4.Foo"] != "1" {
		t.Fatalf("map key が変換されています: tmpfs=%v sysctls=%v", hc.Tmpfs, hc.Sysctls)
	}
	if !hc.Privileged || len(hc.Binds) != 1 || hc.Runtime != "custom-runtime" {
		t.Fatalf("任意 HostConfig field が失われています: %+v", hc)
	}
}

func TestLoad_HostConfigResourcesAreFlat(t *testing.T) {
	c, _ := loadDoc(t, hostConfigDoc(t, "    memory: 4096\n    pidsLimit: 64\n"))
	if c.Runner.HostConfig.Memory != 4096 || c.Runner.HostConfig.PidsLimit == nil || *c.Runner.HostConfig.PidsLimit != 64 {
		t.Fatalf("Resources の flat field が不正です: %+v", c.Runner.HostConfig.Resources)
	}

	_, _, err := Load(writeConfig(t, hostConfigDoc(t, "    resources:\n      memory: 4096\n")))
	checkErr(t, "Resources key", "unknown HostConfig field", err)
}

func TestLoad_HostConfigErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"unknown field", "    noSuchField: true\n", "unknown HostConfig field"},
		{"type mismatch", "    privileged: [true]\n", "invalid HostConfig value"},
		{"duplicate exact", "    capAdd: [CHOWN]\n    capAdd: [DAC_OVERRIDE]\n", "duplicate HostConfig field"},
		{"duplicate case", "    capAdd: [CHOWN]\n    CAPADD: [DAC_OVERRIDE]\n", "duplicate HostConfig field"},
		{"nested unknown", "    restartPolicy:\n      noSuchField: always\n", "unknown HostConfig field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, hostConfigDoc(t, tt.body)))
			checkErr(t, tt.name, tt.want, err)
		})
	}
}

func TestLoad_HostConfigEmptyObjectIsDistinctFromOmitted(t *testing.T) {
	omitted, _ := loadDoc(t, baseWithKey(t))
	empty, _ := loadDoc(t, hostConfigDoc(t, ""))
	if omitted.Runner.HostConfig != nil {
		t.Fatal("HostConfig 未指定なのに nil ではありません")
	}
	if empty.Runner.HostConfig == nil {
		t.Fatal("空の HostConfig が nil です")
	}
	if !reflect.DeepEqual(*empty.Runner.HostConfig, container.HostConfig{}) {
		t.Fatalf("空の HostConfig が空 struct ではありません: %+v", *empty.Runner.HostConfig)
	}
}
