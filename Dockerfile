# ==============================================================
# Teammate Agent Daemon — multi-stage Docker build (with China mirror support)
#
# Build stage: golang:1.26-alpine compiles the Go binary (CGO disabled)
# Runtime stage: alpine:3.21 with ca-certificates, tzdata and git — task
# execution shells out to git, so it is part of the base image.
#
# Build:
#   docker build -t teammate-agentd:latest .
#
# Build with China mirror acceleration:
#   docker build -t teammate-agentd:latest \
#     --build-arg GOPROXY=https://goproxy.cn,direct \
#     --build-arg ALPINE_REPO=mirrors.tuna.tsinghua.edu.cn .
#
# Run (config + workspace trees live in one mounted volume):
#   docker volume create agentd-data
#   docker run -d --name agentd -v agentd-data:/root/.teammate \
#     teammate-agentd:latest
#
# First-time bootstrap of the mounted config (interactive):
#   docker run -it --rm -v agentd-data:/root/.teammate teammate-agentd:latest \
#     workspace add <td_token> --name team-a
#
# Coding-tool CLIs (claude / openclaw / opencode / atomcode / mimocode) are NOT
# bundled: the daemon starts and registers fine without them, and the server
# keeps its instances pending until the tools exist. Provide them either by
# deriving an image that installs the CLIs onto PATH, or by pointing the
# tools.<provider>.path config keys at binaries mounted into the container.
#
# The local control console (local.enabled) binds loopback only and is not
# reachable through published ports; containerized configs should keep
# "local: { enabled: false }".
# ==============================================================

# ---- Build args ----
ARG GO_IMAGE=golang:1.26-alpine
ARG RUNTIME_IMAGE=alpine:3.21

# ---- Build stage ----
FROM $GO_IMAGE AS builder

ARG GOPROXY
# China users can pass --build-arg GOPROXY=https://goproxy.cn,direct
RUN if [ -n "$GOPROXY" ]; then go env -w GOPROXY="$GOPROXY"; fi

WORKDIR /build

# Copy dependency files first to maximize build cache
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build (CGO_ENABLED=0, no gcc needed)
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /teammate-agentd ./cmd/teammate-agentd

# ---- Runtime stage ----
FROM $RUNTIME_IMAGE

ARG ALPINE_REPO
# China users can pass --build-arg ALPINE_REPO=mirrors.aliyun.com
RUN if [ -n "$ALPINE_REPO" ]; then \
      sed -i "s|dl-cdn.alpinelinux.org|$ALPINE_REPO|g" /etc/apk/repositories; \
    fi

# CA certificates (HTTPS to the server), timezone data, and git (task
# execution). safe.directory=* keeps git usable when the workspace volume is
# owned by a host uid different from the container user.
RUN apk add --no-cache ca-certificates tzdata git && \
    update-ca-certificates && \
    git config --global --add safe.directory '*'

COPY --from=builder /teammate-agentd /usr/local/bin/teammate-agentd

# No published ports: the daemon dials out to the server (REST/SSE); the local
# control console is loopback-only and disabled in containerized configs.

# ENTRYPOINT is fixed to the daemon binary; with no CMD arguments the root
# command runs the supervisor. Other subcommands append to it:
#   docker run --rm teammate-agentd:latest agent list
#   docker run --rm teammate-agentd:latest config path
ENTRYPOINT ["teammate-agentd"]
CMD []
