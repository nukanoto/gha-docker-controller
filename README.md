# gha-docker-controller

[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

## 概要

`gha-docker-controller` は、単一の Linux Docker host で GitHub Actions Runner Scale Set の JIT ephemeral runner を管理する daemon です。
1 個の runner container が 1 job を実行し、job の終了後に削除されます。

> [!WARNING]
> controller または runner が処理中に停止した job は、自動で再配信されない場合があります。
> 失敗した job は GitHub Actions から再実行してください。

主な機能:

- 公式 `github.com/actions/scaleset` listener による job message の受信
- `TotalAssignedJobs` に応じた runner container の増減
- GitHub App または `GITHUB_TOKEN` による認証
- 非 privileged な `standard` runner
- gVisor 内で Docker daemon を使う `nested-docker` runner
- 起動時の managed container 回収
- `/livez` と `/readyz` による health check
- systemd による自動再起動と graceful shutdown

Kubernetes、database、webhook、Workflow Runs API、独自 job queue、Docker CLI は使用しません。

runner 数は次の式で決まります。

```text
desired = clamp(max(minRunners, TotalAssignedJobs), 0, maxRunners)
```

不足分は 1 runner ずつ作成します。
超過時は現在の process が作成した idle runner だけを削除し、busy runner と再起動後に保護した running runner は削除しません。

## 動作環境

- Linux host 1 台
- Docker Engine 28.0.0 以上
- Docker API 1.42 以上
- GitHub.com の organization または repository
- Go 1.25.3
- gVisor の `runsc` を使う場合は Docker runtime への登録

GHES、`ghe.com`、`github.localhost` には対応していません。
1 個の controller process は 1 個の Scale Set を管理します。
同じ host では controller を 2 process 起動できません。

## インストール

### Docker と gVisor

Docker Engine を install して起動します。

```bash
docker version
docker info
```

既定の runtime は gVisor の `runsc` です。
次の例では gVisor release archive を checksum 検証して install します。

```bash
GVISOR_RELEASE=20260727
GVISOR_ARCH="$(uname -m)"
GVISOR_URL="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_RELEASE}/${GVISOR_ARCH}"

case "${GVISOR_ARCH}" in
  x86_64|aarch64) ;;
  *) echo "unsupported architecture: ${GVISOR_ARCH}" >&2; exit 1 ;;
esac

curl --fail --location --remote-name "${GVISOR_URL}/gvisor.tar.bz2"
curl --fail --location --remote-name "${GVISOR_URL}/gvisor.tar.bz2.sha512"
sha512sum -c gvisor.tar.bz2.sha512
sudo tar -xjf gvisor.tar.bz2 -C /usr/local/bin
sudo /usr/local/bin/runsc install
sudo systemctl restart docker
```

`runsc` が登録されたことを確認します。

```bash
docker info --format '{{json .Runtimes}}'
```

`standard` profile は登録済みの `runc` なども指定できます。
`nested-docker` profile は `runsc` が必須です。

### binary

repository を取得し、Go 1.25.3 で build します。
build の前に `go env GOVERSION` が `go1.25.3` であることを確認します。

```bash
test "$(go env GOVERSION)" = "go1.25.3" || { echo "error: go env GOVERSION=$(go env GOVERSION), want go1.25.3" >&2; exit 1; }
mkdir -p bin
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w \
    -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Version=$(git describe --tags --always --dirty 2>/dev/null || echo dev) \
    -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Commit=$(git rev-parse --short HEAD 2>/dev/null || echo unknown) \
    -X github.com/nukanoto/gha-docker-controller/internal/buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  -o bin/gha-docker-controller ./cmd/gha-docker-controller
./bin/gha-docker-controller version
```

専用 user を作成し、Docker socket の group へ追加します。

```bash
sudo useradd --system \
  --home-dir /nonexistent \
  --shell /usr/sbin/nologin \
  gha-docker-controller
sudo usermod -aG docker gha-docker-controller
```

Docker socket の group が `docker` ではない場合は、実際の group 名を使います。
systemd unit の `SupplementaryGroups` も同じ group 名へ変更してください。

binary と設定 directory を配置します。

```bash
sudo install -m 0755 \
  bin/gha-docker-controller \
  /usr/local/bin/gha-docker-controller
sudo install -d \
  -o gha-docker-controller \
  -g gha-docker-controller \
  -m 0750 \
  /etc/gha-docker-controller
```

## GitHub 認証

GitHub App と PAT のどちらか一方を使います。
GitHub App と `GITHUB_TOKEN` が同時に設定されている場合、controller は起動しません。

### GitHub App

organization scope では `Self-hosted runners: Read and write` を付与します。
repository scope では `Administration: Read and write` と `Metadata: Read-only` を付与します。

App を対象の organization または repository へ install し、App ID、installation ID、private key を取得します。
private key は専用 file に配置します。

```bash
sudo install \
  -o gha-docker-controller \
  -g gha-docker-controller \
  -m 0600 \
  /path/to/github-app.pem \
  /etc/gha-docker-controller/github-app.pem
```

### PAT

PAT は環境変数 `GITHUB_TOKEN` で渡します。

fine-grained PAT の permission:

- repository scope: `Administration: Read and write`
- organization scope: `Administration: Read` と `Self-hosted runners: Read and write`

classic PAT の scope:

- repository scope: `repo`
- organization scope: `admin:org`

systemd では optional な `EnvironmentFile` から読み込みます。

```bash
sudo install \
  -o gha-docker-controller \
  -g gha-docker-controller \
  -m 0600 \
  /dev/null \
  /etc/gha-docker-controller/environment
sudoedit /etc/gha-docker-controller/environment
```

```text
GITHUB_TOKEN=github_pat_xxxxxxxxxxxx
```

PAT を process の引数、YAML、journal へ書かないでください。

## 設定

`config.example.yaml` を配置して編集します。

```bash
sudo install \
  -o gha-docker-controller \
  -g gha-docker-controller \
  -m 0640 \
  config.example.yaml \
  /etc/gha-docker-controller/config.yaml
sudoedit /etc/gha-docker-controller/config.yaml
```

GitHub App を使う `standard` profile の例です。

```yaml
github:
  url: https://github.com
  scope: organization
  owner: your-organization
  app:
    id: 123456
    installationId: 654321
    privateKeyFile: /etc/gha-docker-controller/github-app.pem

scaleSet:
  name: production
  runnerGroup: default
  minRunners: 0
  maxRunners: 4

docker:
  host: unix:///var/run/docker.sock
  runtime: runsc
  network: bridge
  pullPolicy: if-not-present

runner:
  image: ghcr.io/actions/actions-runner@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda
  profile: standard
  cpu: "2"
  memory: 4GiB
  memorySwap: 4GiB
  pidsLimit: 512
  capDrop: ["ALL"]
  noNewPrivileges: true

health:
  listen: 127.0.0.1:8080

shutdown:
  busyRunnerPolicy: leave
  gracePeriod: 2m

log:
  format: json
  level: info
```

PAT を使う場合は `github.app` を省略します。
`GITHUB_TOKEN` は YAML に書きません。

```yaml
github:
  url: https://github.com
  scope: organization
  owner: your-organization
```

repository scope では `github.repository` も指定します。
`scaleSet.name` は workflow の `runs-on` に使う名前です。
`maxRunners` は 1 以上かつ `minRunners` 以上にします。
`memorySwap` は `memory` 以上にします。

`runner.image` に `latest` は使用できません。
`standard` profile は version tag または digest、`nested-docker` profile は digest が必要です。

resource、DNS、extra hosts、tmpfs、ulimit、seccomp、AppArmor の設定例は `config.example.yaml` にあります。

## 起動前の確認

service account と同じ権限で `check` を実行します。
PAT を使う場合は `GITHUB_TOKEN` が必要です。

GitHub App:

```bash
sudo -u gha-docker-controller \
  /usr/local/bin/gha-docker-controller check \
  --config /etc/gha-docker-controller/config.yaml
```

PAT:

```bash
sudo -u gha-docker-controller sh -c '
  set -a
  . /etc/gha-docker-controller/environment
  exec /usr/local/bin/gha-docker-controller check \
    --config /etc/gha-docker-controller/config.yaml
'
```

`check` は config、認証、runner group、既存 Scale Set、Docker、runtime、network、runner image、container resource を検証します。
container、runner、Scale Set、network は作成または削除しません。
ただし pull policy に従って image を pullする場合があります。

Scale Set が存在しない場合は warning になります。
`serve` は初回起動時に Scale Set を作成します。

## systemd

同梱の unit file を install して起動します。

```bash
sudo install -m 0644 \
  deploy/gha-docker-controller.service \
  /etc/systemd/system/gha-docker-controller.service
sudo systemctl daemon-reload
sudo systemctl enable --now gha-docker-controller
```

状態を確認します。

```bash
sudo systemctl status gha-docker-controller
sudo journalctl -u gha-docker-controller -n 100 --no-pager
curl --fail http://127.0.0.1:8080/livez
curl --fail http://127.0.0.1:8080/readyz
```

`/livez` は process が生存していれば 200 を返します。
`/readyz` は GitHub message session と listener が稼働している場合だけ 200 を返します。

log は既定で JSON、message は英語です。
credential、JIT config、Authorization、secret response body は log と health response に出力しません。

同じ host で 2 process 目を起動すると、`/run/gha-docker-controller/controller.lock` の lock 取得に失敗します。

## workflow

controller が ready になった後、対象 repository に workflow を追加します。
`runs-on` は config の `scaleSet.name` と一致させます。

```yaml
name: runner check

on:
  workflow_dispatch:

permissions:
  contents: read

jobs:
  check:
    runs-on: production
    steps:
      - uses: actions/checkout@v4
      - run: |
          uname -a
          echo "runner is ready"
```

GitHub Actions は `actions/checkout@v4` のような tag で動作します。
更新を自動で受け取りたい場合は version tag、実行内容を固定したい場合は commit SHA を使います。

各 runner は 1 job だけを実行します。
workspace、導入した package、local cache は次の job へ引き継がれません。
job 間で data を渡す場合は artifact、cache、registry などの外部 storage を使います。

### standard profile

`standard` runner は host の Docker socket を持ちません。
次の機能には対応していません。

- `docker build` と `docker run`
- Docker container action
- job の `container`
- job の `services`

Docker が必要な workflow には `nested-docker` profile を使います。
runner image は GitHub-hosted runner の `ubuntu-latest` と同じ tool 一式を保証しません。

### nested-docker profile

`nested-docker` は gVisor sandbox 内で独立した Docker daemon を起動します。
この profile を使う host の `runsc` runtime に次の argument を設定します。

```json
{
  "runtimes": {
    "runsc": {
      "path": "/usr/local/bin/runsc",
      "runtimeArgs": [
        "--net-raw",
        "--allow-packet-socket-write"
      ]
    }
  }
}
```

Docker API から runtime argument を取得できないため、host の `daemon.json` を直接確認してください。
設定後に Docker daemon を再起動します。

専用 runner image を build して registry へ push します。

```bash
docker build -t ghcr.io/example/gha-nested-runner:29.6.1 images/nested-docker
docker push ghcr.io/example/gha-nested-runner:29.6.1
docker inspect --format='{{index .RepoDigests 0}}' \
  ghcr.io/example/gha-nested-runner:29.6.1
```

nested 用 config の主要部分です。
`runner.image` は push 後に得た digest へ置き換えます。

```yaml
scaleSet:
  name: production-nested
  runnerGroup: default
  minRunners: 0
  maxRunners: 2

docker:
  host: unix:///var/run/docker.sock
  runtime: runsc
  network: bridge
  pullPolicy: if-not-present

runner:
  image: ghcr.io/example/gha-nested-runner@sha256:<digest>
  profile: nested-docker
  cpu: "4"
  memory: 8GiB
  memorySwap: 8GiB
  pidsLimit: 1024
  capDrop: ["ALL"]
  noNewPrivileges: true

nestedDocker:
  storage: tmpfs
  storageSize: 16GiB
```

job container と service container は `--network host` を使います。
service には `127.0.0.1:<port>` で接続します。

```yaml
jobs:
  integration:
    runs-on: production-nested
    container:
      image: ghcr.io/example/job-image@sha256:<digest>
      options: --network host
    services:
      postgres:
        image: postgres@sha256:<digest>
        options: --network host
        env:
          POSTGRES_PASSWORD: test
    steps:
      - uses: actions/checkout@v4
      - run: docker version
      - run: docker build -t application:test .
      - run: docker run --rm --network host application:test
```

inner Docker daemon は iptables と ip6tables を使いません。
`services.<name>.ports`、`docker run -p`、`--publish`、`--expose` は使用できません。
Docker Compose を使う場合は各 service に `network_mode: host` を指定し、top-level の `networks` を空にします。

`/var/lib/docker` は tmpfs です。
Docker image、volume、container は job の終了時に破棄されます。

## 運用

### 停止と更新

```bash
sudo systemctl stop gha-docker-controller
```

既定の `shutdown.busyRunnerPolicy: leave` では busy runner を停止しません。
再起動時に running だった managed runner は `protected` として残り、終了監視だけを再開します。
created、exited、dead の managed container は起動時に削除します。

binary を更新する場合は停止後に置き換え、`check` を実行してから起動します。

```bash
sudo systemctl stop gha-docker-controller
sudo install -m 0755 \
  bin/gha-docker-controller \
  /usr/local/bin/gha-docker-controller
sudo systemctl start gha-docker-controller
sudo systemctl status gha-docker-controller
```

### 障害対応

```bash
sudo systemctl status gha-docker-controller
sudo journalctl -u gha-docker-controller -n 200 --no-pager
curl -i http://127.0.0.1:8080/readyz
docker info --format '{{.ServerVersion}} {{json .Runtimes}}'
docker ps -a --filter label=managed=true
```

job が `Queued` のままの場合は、workflow の `runs-on`、`/readyz`、`maxRunners` を確認します。
runner が起動しない場合は managed container の log を確認します。

```bash
docker logs --tail=200 <container-id>
```

`docker inspect` の Env には JIT config が含まれるため、出力を共有しないでください。

終了しない `protected` container は operator が停止します。

```bash
docker stop <container-id>
```

exit 監視が停止を検知すると controller が container を削除します。
managed label のない container、別 Scale Set の container、label が壊れた container は変更しません。

controller または runner の crash で失敗した job は再実行します。

```bash
gh run rerun <run-id> --failed
```

## controller container

native systemd を推奨します。
controller container を使う場合は Docker socket、lock directory、config directory を mount します。
PAT は `GITHUB_TOKEN` で渡します。

container の health endpoint は `0.0.0.0:8080` で listen し、公開先を host loopback に限定します。

```yaml
health:
  listen: 0.0.0.0:8080
```

```bash
docker build -t gha-docker-controller:local .
sudo install -d -m 0750 /run/gha-docker-controller

docker run -d \
  --name gha-docker-controller \
  --restart on-failure \
  -e GITHUB_TOKEN \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /run/gha-docker-controller:/run/gha-docker-controller \
  -v /etc/gha-docker-controller:/etc/gha-docker-controller:ro \
  -p 127.0.0.1:8080:8080 \
  gha-docker-controller:local
```

controller container は host の Docker control 権限を持ちます。
image の実行権限を operator だけに限定してください。

## ライセンス

MIT License
