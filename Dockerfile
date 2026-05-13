# =========================================================
# Builder Stage
# =========================================================
FROM golang:1.24 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download

COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/
COPY reconcilers/ reconcilers/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -o manager cmd/main.go

# =========================================================
# Runtime Stage
# =========================================================
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y \
    bash \
    curl \
    wget \
    git \
    ca-certificates \
    buildah \
    skopeo \
    fuse-overlayfs \
    uidmap \
    iptables \
    containernetworking-plugins \
    && rm -rf /var/lib/apt/lists/*

# Create checkpoint directory
RUN mkdir -p /var/lib/kubelet/checkpoints

# Buildah storage configuration
RUN mkdir -p /etc/containers

RUN printf '[storage]\ndriver = "vfs"\nrunroot = "/tmp/runroot"\ngraphroot = "/tmp/graphroot"\n' \
    > /etc/containers/storage.conf

WORKDIR /

COPY --from=builder /workspace/manager .

RUN useradd -u 65532 -m appuser

RUN chown -R 65532:65532 /tmp

USER 65532:65532

ENTRYPOINT ["/manager"]