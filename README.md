# gha-docker-controller

## Overview

`gha-docker-controller` is an application that runs ephemeral GitHub Actions runners in containers.

## Features

* Runs each job in an isolated container without requiring a cluster
* Allows Docker to run inside the runner when using gVisor (DinD)

## Requirements

* Docker Engine v28.0.0 or later
* [`runsc`](https://gvisor.dev/docs/user_guide/install/) (required when using DinD)

## Installation

### GitHub Authentication

#### GitHub App

Grant the following permissions:

* Organization scope

  * `Self-hosted runners: Read and write`
* Repository scope

  * `Administration: Read and write`
  * `Metadata: Read-only`

#### PAT

Pass the PAT through the `GITHUB_TOKEN` environment variable.

Permissions required for a fine-grained PAT:

* Repository scope

  * `Administration: Read and write`
* Organization scope

  * `Administration: Read`
  * `Self-hosted runners: Read and write`

```bash
export GITHUB_TOKEN=github_pat_xxxxxxxxxxxx
```

### Run

You can run the application using Docker Compose.

```yaml
```

## Configuration

The following is an example of the `standard` profile using a GitHub App.

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

When using a PAT, omit `github.app`.
Do not include `GITHUB_TOKEN` in the YAML file.

```yaml
github:
  url: https://github.com
  scope: organization
  owner: your-organization
```

Keep the following points in mind when configuring the application:

* For repository scope, also specify `github.repository`
* `scaleSet.name` is the value used for `runs-on` in workflows
* `maxRunners` must be at least 1 and greater than or equal to `minRunners`
* `memorySwap` must be greater than or equal to `memory`
* `latest` cannot be used for `runner.image`
* The `standard` profile requires either a version tag or a digest
* The `dind-runner` profile requires a digest

Examples of resource, DNS, extra hosts, tmpfs, ulimit, seccomp, and AppArmor settings are available in `config.example.yaml`.

## Preflight Check

```bash
gha-docker-controller check --config /path/to/config.yaml
```

## Workflow

After the controller becomes ready, add a workflow to the target repository.
The value of `runs-on` must match `scaleSet.name` in the configuration.

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

### `dind-runner` Profile

The `dind-runner` profile starts an independent Docker daemon inside the runner.

Configure the host's `/etc/docker/daemon.json` as follows:

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

Modify `config.yaml` as follows:

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
  noNewPrivileges: true

dindRunner:
  storage: tmpfs
  storageSize: 16GiB
```

> [!NOTE]
> The inner Docker daemon does not use iptables or ip6tables.
>
> `services.<name>.ports`, `docker run -p`, `--publish`, and `--expose` cannot be used.
>
> When using Docker Compose, specify `network_mode: host` for each service.

## Build

```bash
go build ./cmd/gha-docker-controller
```
