FROM mcr.microsoft.com/devcontainers/go:1.26-bookworm

USER root

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl git make \
    && rm -rf /var/lib/apt/lists/*

RUN GOBIN=/usr/local/bin go install github.com/bufbuild/buf/cmd/buf@v1.68.4

RUN mkdir -p /go/pkg /home/vscode/.cache/go-build \
    && chown -R vscode:vscode /go /home/vscode/.cache

USER vscode
