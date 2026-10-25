# syntax=docker/dockerfile:1
# Create a stage for building the application.
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION} AS build
WORKDIR /src
ENV PKG_CONFIG_PATH="" LD_LIBRARY_PATH=""

# libdave (Discord DAVE E2EE voice encryption) is a CGO dependency of
# github.com/disgoorg/godave/libdave; install the native shared lib + headers.
ARG LIBDAVE_VERSION=v1.1.0
RUN apt-get update && apt-get install -y --no-install-recommends curl unzip && rm -rf /var/lib/apt/lists/*
ADD https://raw.githubusercontent.com/disgoorg/godave/master/scripts/libdave_install.sh /tmp/libdave_install.sh
RUN chmod +x /tmp/libdave_install.sh && SHELL=/bin/sh NON_INTERACTIVE=1 /tmp/libdave_install.sh "${LIBDAVE_VERSION}"
ENV PKG_CONFIG_PATH="/root/.local/lib/pkgconfig:${PKG_CONFIG_PATH}"
ENV LD_LIBRARY_PATH="/root/.local/lib:${LD_LIBRARY_PATH}"

# Download dependencies as a separate step to take advantage of Docker's caching.
# Leverage a cache mount to /go/pkg/mod/ to speed up subsequent builds.
# Leverage bind mounts to go.sum and go.mod to avoid having to copy them into
# the container.
# Cache dependencies. GOMODCACHE=/go/pkg/mod
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download

# Build. GOCACHE=/root/.cache/go-build
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=.,target=. \
    CGO_ENABLED=1 go build -trimpath -o /bin/dbot ./cmd/dbot

################################################################################
FROM archlinux:latest AS final

RUN --mount=type=cache,target=/var/cache/pacman/pkg/ \
    pacman -Syu --noconfirm --needed \
    tzdata \
    ffmpeg \
    deno \
    libvpl vpl-gpu-rt \
    wget \
    python3

# install ytdlp from GH — standalone build bundles curl_cffi, needed for
# --impersonate firefox (reddit TLS fingerprint check); plain asset omits it
RUN wget https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp_linux && \
    chmod +x ./yt-dlp_linux && \
    mv ./yt-dlp_linux /usr/bin/yt-dlp

WORKDIR /dbot
COPY --from=build /bin/dbot .
COPY --from=build /root/.local/lib/libdave.so /usr/lib/libdave.so
RUN chmod +x ./dbot && ldconfig

# CMD [ "sleep","90900900" ]
CMD [ "/dbot/dbot" ]
