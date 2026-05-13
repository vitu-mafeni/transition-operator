# =========================
# Builder
# =========================
FROM golang:1.24 AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Go dependencies
COPY go.mod go.mod
COPY go.sum go.sum

RUN go mod download

# Source
COPY cmd/main.go cmd/main.go
COPY api/ api/
COPY internal/ internal/
COPY reconcilers/ reconcilers/

# Build binary
RUN CGO_ENABLED=0 \
    GOOS=${TARGETOS:-linux} \
    GOARCH=${TARGETARCH} \
    go build -a -o manager cmd/main.go


# =========================
# Production Image
# =========================
# FROM gcr.io/distroless/static:nonroot AS production

# WORKDIR /

# COPY --from=builder /workspace/manager .

# USER 65532:65532

# ENTRYPOINT ["/manager"]


# =========================
# Debug Image
# =========================
FROM alpine:3.20 AS debug

WORKDIR /

COPY --from=builder /workspace/manager .

RUN apk add --no-cache \
    bash \
    curl \
    busybox-extras

# Create non-root user/group with fixed numeric IDs
RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

USER 1001:1001

ENTRYPOINT ["/manager"]