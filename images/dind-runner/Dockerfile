FROM ghcr.io/actions/actions-runner

USER root

# Install only tools required by the DinD entrypoint.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        iproute2 \
        iptables \
        util-linux \
        curl \
    && rm -rf /var/lib/apt/lists/*

# Fail the build if the base image ships another Docker version.
RUN set -eu; \
    version="$(dockerd --version)"; \
    case "$version" in \
        "Docker version 29.6.1,"*) ;; \
        *) printf 'unexpected dockerd version: %s\n' "$version" >&2; exit 1 ;; \
    esac

COPY entrypoint.sh /usr/local/bin/gha-dind-entrypoint
RUN chmod 0755 /usr/local/bin/gha-dind-entrypoint

USER root
ENTRYPOINT ["/usr/local/bin/gha-dind-entrypoint"]
