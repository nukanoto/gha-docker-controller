FROM ghcr.io/actions/actions-runner

USER root

# Add only the tools needed to run dind Docker.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        iproute2 \
        iptables \
        util-linux \
        curl \
    && rm -rf /var/lib/apt/lists/*

# Verify the dockerd version shipped in the official runner image at
# build time.
RUN set -eu; \
    version="$(dockerd --version)"; \
    case "$version" in \
        "Docker version 29.6.1,"*) ;; \
        *) printf 'unexpected dockerd version: %s\n' "$version" >&2; exit 1 ;; \
    esac

COPY entrypoint.sh /usr/local/bin/gha-dind-entrypoint
RUN chmod 0755 /usr/local/bin/gha-dind-entrypoint

LABEL io.github.nukanoto.gha-docker-controller.profile="dind-runner-v1" \
      io.github.nukanoto.gha-docker-controller.inner-docker="29.6.1"

USER root
ENTRYPOINT ["/usr/local/bin/gha-dind-entrypoint"]
