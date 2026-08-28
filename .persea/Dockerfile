# Environment the Persea debugger runs this repository in: the Go toolchain and
# the system libraries its tests need. It is never the application image — the
# repository is mounted at /workspace and the setup, build and test commands are
# run against it, so no ENTRYPOINT or CMD of ours would ever execute.
FROM golang:1.21-bookworm

# gorm.io/driver/sqlite is cgo-backed, so the suite needs a C toolchain.
RUN apt-get update \
    && apt-get install --no-install-recommends -y \
        build-essential \
        ca-certificates \
        git \
    && rm -rf /var/lib/apt/lists/*

ENV CGO_ENABLED=1 \
    GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false

WORKDIR /workspace
