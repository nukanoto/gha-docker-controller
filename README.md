# arc-docker

## Overview

`arc-docker` is a Docker-based controller for GitHub Actions Runner Scale Sets.
It uses the official Actions Scale Set listener to provision ephemeral runners
as Docker containers.

## Features

* Runs each job in an isolated container without requiring a cluster
* Allows Docker to run inside the runner when using gVisor (DinD)

## Requirements

* Docker Engine v28.0.0 or later
* [`runsc`](https://gvisor.dev/docs/user_guide/install/) (required when using DinD)

## Installation

You can run the application using Docker Compose.

```yaml
services:
  controller:
    image: ghcr.io/nukanoto/arc-docker:latest
    pull_policy: always
    restart: on-failure
    stop_grace_period: 8m
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./config.yaml:/etc/arc-docker/config.yaml:ro
      - ./arc-docker-github-app.pem:/etc/arc-docker/github-app.pem
```

## GitHub Authentication

### GitHub App

Grant the following permissions:

* Organization scope

  * `Self-hosted runners: Read and write`
* Repository scope

  * `Administration: Read and write`
  * `Metadata: Read-only`

### PAT

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

## Configuration

The following is an example configuration using a GitHub App.

```yaml
github:
  url: https://github.com
  scope: organization
  owner: your-organization
  app:
    id: 123456
    installationId: 654321
    privateKeyFile: /etc/arc-docker/github-app.pem
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
  # HostConfig keys use Docker's API names without case sensitivity.
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

When using a PAT, omit `github.app`.
Do not include `GITHUB_TOKEN` in the YAML file.

```yaml
github:
  url: https://github.com
  scope: organization
  owner: your-organization
```

## Preflight Check

```bash
arc-docker check --config /path/to/config.yaml
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

### DinD Usage

To use Docker-in-Docker, provide an image that starts an independent Docker
daemon and configure the image's requirements under `runner.hostConfig`.

The repository includes separate runner images for each outer runtime:

* [`images/dind-runner-runsc/`](images/dind-runner-runsc/) for gVisor
* [`images/dind-runner-sysbox/`](images/dind-runner-sysbox/) for Sysbox

### runsc (gVisor)

Configure the host's `/etc/docker/daemon.json` as follows.

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

Configure the `runner` section in `config.yaml` as follows:

```yaml
runner:
  image: ghcr.io/example/gha-dind-runner-runsc@sha256:<digest>
  hostConfig:
    runtime: runsc
    capDrop: [ALL]
    capAdd:
      - AUDIT_WRITE
      - CHOWN
      - DAC_OVERRIDE
      - FOWNER
      - FSETID
      - KILL
      - MKNOD
      - NET_ADMIN
      - NET_BIND_SERVICE
      - NET_RAW
      - SETFCAP
      - SETGID
      - SETPCAP
      - SETUID
      - SYS_ADMIN
      - SYS_CHROOT
    securityOpt: [no-new-privileges]
    mounts:
      - type: tmpfs
        target: /var/lib/docker
        tmpfsOptions:
          sizeBytes: 17179869184
          mode: 448
          options:
            - [exec]
    pidsLimit: 1024
```

The required capabilities and `/var/lib/docker` storage depend on the image
and workload. This example only sets `pidsLimit` as a resource limit; CPU and
memory are optional.

> [!NOTE]
> The example image disables iptables and ip6tables for the Docker daemon inside the runner.
>
> `services.<name>.ports`, `docker run -p`, `--publish`, and `--expose` cannot be used.
>
> Specify `options: --network host` for containers.

### Sysbox

Install Sysbox on the Docker host and register the `sysbox-runc` runtime.
Use the Sysbox runner image with the runtime selected in `HostConfig`.

```yaml
runner:
  image: ghcr.io/example/gha-dind-runner-sysbox@sha256:<digest>
  hostConfig:
    runtime: sysbox-runc
    mounts:
      - type: tmpfs
        target: /var/lib/docker
        tmpfsOptions:
          sizeBytes: 17179869184
          mode: 448
          options:
            - [exec]
    pidsLimit: 1024
```

Do not set `privileged`, `SYS_ADMIN`, or the runsc-specific capability and
network workarounds for the Sysbox image. The image uses the inner Docker
daemon's normal networking.

## Build

```bash
go build ./cmd/arc-docker
```
