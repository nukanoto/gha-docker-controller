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
services:
  controller:
    image: ghcr.io/nukanoto/gha-docker-controller:latest
    pull_policy: always
    restart: on-failure
    stop_grace_period: 8m
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./config.yaml:/etc/gha-docker-controller/config.yaml:ro
      - ./gha-github-app.pem:/etc/gha-docker-controller/github-app.pem
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

GitHub App を使う設定例です。

```yaml
github:
  url: https://github.com
  scope: organization
  owner: your-organization
  app:
    id: 123456
    installationId: 654321
    privateKeyFile: /etc/gha-docker-controller/github-app.pem
# repository: "<repository name>"

scaleSet:
  name: my-self-hosted-runner
  runnerGroup: default
  minRunners: 0
  maxRunners: 4

docker:
  host: unix:///var/run/docker.sock
  pullPolicy: if-not-present

runner:
  image: ghcr.io/actions/actions-runner@sha256:<digest>
  hostConfig:
    pidsLimit: 512
  provisioningTimeout: 5m
  stopTimeout: 30s

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

### DinD の利用

Docker-in-Docker を使う場合は、独立した Docker daemon を起動する image を用意し、
その image に必要な設定を `runner.hostConfig` に指定します。

リポジトリには [`images/dind-runner/`](images/dind-runner/) に Dockerfile の例があります。

### runsc (gVisor) の場合

host の `/etc/docker/daemon.json` に以下のように設定します。

```json
{
  "runtimes": {
    "runsc": {
      "path": "/usr/bin/runsc",
      "runtimeArgs": [
        "--net-raw",
        "--allow-packet-socket-write"
      ]
    }
  }
}
```

`config.yaml` の `runner` を以下のように設定します。

```yaml
runner:
  image: ghcr.io/example/gha-dind-runner@sha256:<digest>
  hostConfig:
    runtime: runsc
    capDrop: [ALL]
    capAdd: [NET_ADMIN, NET_RAW, SYS_ADMIN]
    securityOpt: [no-new-privileges]
    mounts:
      - type: tmpfs
        target: /var/lib/docker
        tmpfsOptions:
          sizeBytes: 17179869184
          mode: 448
    pidsLimit: 1024
```

必要な capability と `/var/lib/docker` の storage は image と workload に依存します。
この例では resource limit として `pidsLimit` だけを指定し、CPU と memory は任意としています。

> [!NOTE]
> 例に挙げた image は runner 内の Docker daemon で iptables と ip6tables を無効にします。
>
> `services.<name>.ports`、`docker run -p`、`--publish`、`--expose` は使用できません。
>
> コンテナには `options: --network host` を指定してください。

## Build

```bash
go build ./cmd/gha-docker-controller
```
