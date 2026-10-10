# Self-hosted GitHub Actions runner for dbot's own CI.
#
# Based on golang:1.26 (same base as the app's Dockerfile build stage) so the
# Go toolchain, gcc (CGO) and libdave are already present for `go build`/`go
# test` on the rewrite-to-disgo branch, without re-downloading the whole Go
# toolchain inside the runner's job steps.
FROM golang:1.26

ARG RUNNER_VERSION=2.338.0
ARG LIBDAVE_VERSION=v1.1.0

# libicu76: the runner's bundled .NET runtime needs ICU, but its own
# installdependencies.sh only tries package names up to libicu72 and this
# base image (Debian trixie) ships libicu76 - install it explicitly.
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    unzip \
    jq \
    ca-certificates \
    sudo \
    libicu76 \
    && rm -rf /var/lib/apt/lists/*

# libdave (Discord DAVE E2EE voice encryption), same install path as Dockerfile.
RUN curl -fsSL https://raw.githubusercontent.com/disgoorg/godave/master/scripts/libdave_install.sh -o /tmp/libdave_install.sh \
    && chmod +x /tmp/libdave_install.sh \
    && SHELL=/bin/sh NON_INTERACTIVE=1 /tmp/libdave_install.sh "${LIBDAVE_VERSION}"
ENV PKG_CONFIG_PATH="/root/.local/lib/pkgconfig" \
    LD_LIBRARY_PATH="/root/.local/lib"

# Runner must not run its own job steps as root.
RUN useradd -m runner && usermod -aG sudo runner \
    && echo "runner ALL=(ALL) NOPASSWD: ALL" > /etc/sudoers.d/runner

RUN mkdir -p /home/runner/actions-runner
WORKDIR /home/runner/actions-runner

RUN curl -fsSL -o actions-runner.tar.gz \
    "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-x64-${RUNNER_VERSION}.tar.gz" \
    && tar xzf actions-runner.tar.gz \
    && rm actions-runner.tar.gz \
    && ./bin/installdependencies.sh

COPY entrypoint.sh /home/runner/entrypoint.sh
RUN chmod +x /home/runner/entrypoint.sh \
    && chown -R runner:runner /home/runner

USER runner
ENTRYPOINT ["/home/runner/entrypoint.sh"]
