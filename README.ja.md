# gha-docker-controller

## 概要

`gha-docker-controller` は、GitHub Actions の ephemeral runner をコンテナ上で動かすアプリケーションです。

## 特徴

- クラスタを立てなくても、job ごとにコンテナで分離しながら実行できる
- gVisor を利用する場合、runner 上で Docker を扱える（DinD）

## Requirements

- Docker Engine v28.0.0 以上
- [`runsc`](https://gvisor.dev/docs/user_guide/install/)（DinD を使う場合は必須）

## インストール

Docker Compose を利用して実行できます。

```yaml
```

## GitHub 認証

### GitHub App

以下の権限を付与します。

- Organization scope
  - `Self-hosted runners: Read and write`
- Repository scope
  - `Administration: Read and write`
  - `Metadata: Read-only`

### PAT

PAT は環境変数 `GITHUB_TOKEN` で渡します。

fine-grained PAT の permissions:

- Repository scope
  - `Administration: Read and write`
- Organization scope
  - `Administration: Read`
  - `Self-hosted runners: Read and write`

```bash
export GITHUB_TOKEN=github_pat_xxxxxxxxxxxx
```

## 設定

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

設定時は、以下の点に注意してください。

- Repository scope では `github.repository` も指定します
- `scaleSet.name` は workflow の `runs-on` に使う名前です
- `maxRunners` は 1 以上かつ `minRunners` 以上にします
- `cpu`、`memory`、`memorySwap`、`pidsLimit` は省略または `0` で無制限になります
- `memory` と `memorySwap` がともに正数の場合、`memorySwap` は `memory` 以上にします
- `memory` が無制限のとき、正数の `memorySwap` は指定できません
- `runner.image` に `latest` は使用できません
- `standard` profile は version tag または digest が必要です
- `dind-runner` profile は digest が必要です

resource、DNS、extra hosts、tmpfs、ulimit、seccomp、AppArmor の設定例は `config.example.yaml` にあります。

## 起動前の確認

```bash
gha-docker-controller check --config /path/to/config.yaml
```

## Workflow

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

### `dind-runner` profile

`dind-runner` は runner 内で独立した Docker daemon を起動します。

host の `/etc/docker/daemon.json` に以下のように設定します。

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

`config.yaml` を以下のように修正します。

```yaml
scaleSet:
  name: production-dind
  runnerGroup: default
  minRunners: 0
  maxRunners: 2

docker:
  host: unix:///var/run/docker.sock
  runtime: runsc
  network: bridge
  pullPolicy: always

runner:
  image: ghcr.io/example/gha-dind-runner@sha256:<digest>
  profile: dind-runner
  cpu: "4"
  memory: 8GiB
  memorySwap: 8GiB
  pidsLimit: 1024
  capDrop: ["ALL"]

dindRunner:
  storage: tmpfs
  storageSize: 16GiB
```

> [!NOTE]
> runner内の Docker は iptables と ip6tables を使いません。
>
> `services.<name>.ports`、`docker run -p`、`--publish`、`--expose` は使用できません。
>
> Docker Compose を使う場合は、各 service に `network_mode: host` を指定してください。

## Build

```bash
go build ./cmd/gha-docker-controller
```
