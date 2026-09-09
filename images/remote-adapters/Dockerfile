# syntax=docker/dockerfile:1

# Build a remote-capable container image for a Criteria v2 adapter.
#
# The image packages the generic remote runner along with the requested
# adapter binary. At runtime the runner starts the local adapter via
# hashicorp/go-plugin and forwards the v2 contract to a remote host using
# adapterhost.ServeRemote.
#
# Required build arguments:
#   ADAPTER_NAME    - "shell" or "copilot"
#   ADAPTER_VERSION - Semver without leading v (e.g. "0.5.2")
#
# Optional build arguments:
#   BASE_IMAGE      - Runtime base image (default: alpine:3.20)
#   COPILOT_VERSION - npm version of @github/copilot for copilot images
#                     (default: 1.0.82)
#
# Example:
#   docker buildx build \
#     --build-arg ADAPTER_NAME=shell \
#     --build-arg ADAPTER_VERSION=0.5.2 \
#     -f images/remote-adapters/Dockerfile \
#     -t ghcr.io/brokenbots/criteria-adapter-shell:0.5.2 .

ARG ADAPTER_NAME=shell
ARG ADAPTER_VERSION=0.0.0
ARG BASE_IMAGE=alpine:3.20
ARG COPILOT_VERSION=1.0.82

FROM golang:1.26.6-bookworm AS builder

ARG ADAPTER_NAME
ARG ADAPTER_VERSION

WORKDIR /src

COPY go.mod go.sum ./
COPY sdk/ sdk/
COPY workflow/ workflow/
COPY internal/ internal/
COPY cmd/ cmd/

ENV CGO_ENABLED=0
ENV GOWORK=off

RUN go build -trimpath -ldflags="-s -w" \
    -o /usr/local/bin/criteria-adapter-remote-runner \
    ./cmd/criteria-adapter-remote-runner

# Pull the requested adapter binary from its own public module.
RUN go install "github.com/brokenbots/criteria-adapter-${ADAPTER_NAME}@v${ADAPTER_VERSION}"

FROM ${BASE_IMAGE}

ARG ADAPTER_NAME
ARG ADAPTER_VERSION
ARG COPILOT_VERSION

USER root

# Alpine-based images need CA certificates for TLS back to the remote host and
# a shell for the shell adapter. Node-based images used for copilot need the
# same certificate bundle.
RUN case "$(cat /etc/os-release 2>/dev/null | grep -E '^ID=' | cut -d= -f2 | tr -d '"')" in \
      alpine) apk add --no-cache ca-certificates ;; \
      debian) apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/* ;; \
    esac

# The copilot adapter requires the GitHub Copilot CLI on PATH.
RUN if [ "$ADAPTER_NAME" = "copilot" ]; then \
      npm install -g "@github/copilot@${COPILOT_VERSION}" && \
      npm cache clean --force; \
    fi

RUN addgroup -g 10001 -S criteria 2>/dev/null || groupadd --system --gid 10001 criteria && \
    adduser -u 10001 -S criteria -G criteria 2>/dev/null || \
    useradd --system --uid 10001 --gid criteria --home-dir /home/criteria --create-home criteria

COPY --from=builder /usr/local/bin/criteria-adapter-remote-runner /usr/local/bin/
COPY --from=builder /go/bin/criteria-adapter-${ADAPTER_NAME} /usr/local/bin/

ENV CRITERIA_ADAPTER_NAME=${ADAPTER_NAME} \
    CRITERIA_ADAPTER_BINARY=/usr/local/bin/criteria-adapter-${ADAPTER_NAME} \
    CRITERIA_ADAPTER_VERSION=${ADAPTER_VERSION} \
    HOME=/home/criteria

USER criteria
WORKDIR /home/criteria

ENTRYPOINT ["/usr/local/bin/criteria-adapter-remote-runner"]
