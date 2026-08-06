FROM ghcr.io/actions/actions-runner@sha256:0cfdcc701ce933c6d243c6b0b2da767366dc9f2e99961d4c3754b0b78084cdda

USER root

# Add only the tools needed to run nested Docker.
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

COPY entrypoint.sh /usr/local/bin/gha-nested-entrypoint
RUN chmod 0755 /usr/local/bin/gha-nested-entrypoint

LABEL io.github.nukanoto.gha-docker-controller.profile="nested-docker-v1" \
      io.github.nukanoto.gha-docker-controller.inner-docker="29.6.1"

USER root
ENTRYPOINT ["/usr/local/bin/gha-nested-entrypoint"]
