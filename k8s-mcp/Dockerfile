# MIG-7: scoped Kubernetes/gitops MCP server. Self-contained Go module
# (stdlib-only runtime deps), builds from the component directory:
#   docker build -t skquad-k8s-mcp ./k8s-mcp
ARG GO_VERSION=1.26.10

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-k8s-mcp ./cmd/k8s-mcp

# Hardened minimal runtime (mirrors tool-gateway: minimal image, no shell,
# read-only rootfs enforced at the pod securityContext).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/skquad-k8s-mcp /usr/local/bin/skquad-k8s-mcp
EXPOSE 8081
USER 65532:65532
ENTRYPOINT ["skquad-k8s-mcp"]
