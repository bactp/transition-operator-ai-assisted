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
    jq \
    openssl \
    tar \
    && rm -rf /var/lib/apt/lists/*

# kagent CLI - notifyAgentOfFault() in clusterpolicy_controller.go shells out to
# `kagent invoke` to reach the failover-agent. Pinned to the version already
# verified against this deployment's live kagent-controller.
ENV KAGENT_VERSION=v0.10.0-beta10
RUN curl -fsSL https://raw.githubusercontent.com/kagent-dev/kagent/main/scripts/get-kagent -o /tmp/get-kagent.sh \
    && chmod +x /tmp/get-kagent.sh \
    && /tmp/get-kagent.sh --version ${KAGENT_VERSION} \
    && rm /tmp/get-kagent.sh

# kubectl - some kagent CLI paths (e.g. `kagent version` with no --kagent-url)
# fall back to a kubectl port-forward; the operator always passes --kagent-url
# explicitly, but keep kubectl available for parity/debugging.
RUN curl -fsSL "https://dl.k8s.io/release/$(curl -fsSL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" -o /usr/local/bin/kubectl \
    && chmod +x /usr/local/bin/kubectl

# Create checkpoint directory
RUN mkdir -p /var/lib/kubelet/checkpoints

# Buildah storage configuration
RUN mkdir -p /etc/containers

RUN printf '[storage]\ndriver = "vfs"\nrunroot = "/tmp/runroot"\ngraphroot = "/tmp/graphroot"\n' \
    > /etc/containers/storage.conf

WORKDIR /

COPY --from=builder /workspace/manager .

# -g appuser (not the default auto-assigned group) so /etc/passwd's GID
# matches the numeric 65532:65532 the Deployment's USER directive actually
# runs as - newuidmap/newgidmap refuse to trust the subuid/subgid range
# below otherwise ("Target process is owned by a different user"), silently
# falling back to a degraded single-mapping mode that can't do the private
# mount-namespace remount buildah commit needs (verified locally: reproduced
# "remount /, flags: 0x44000: permission denied" down to this exact cause).
RUN groupadd -g 65532 appuser && useradd -u 65532 -g appuser -m appuser

# subuid/subgid range for rootless buildah's user-namespace UID remapping.
RUN echo "appuser:100000:65536" > /etc/subuid \
    && echo "appuser:100000:65536" > /etc/subgid

RUN chown -R 65532:65532 /tmp /var/lib/kubelet/checkpoints

USER 65532:65532

ENTRYPOINT ["/manager"]