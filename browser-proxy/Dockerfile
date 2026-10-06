# syntax=docker/dockerfile:1
# TG-6 browser-proxy: netguard-pinned egress forward proxy sidecar
# (docs/tg6-browser-protocol.md §5).
#
# TG-6: builds from the REPOSITORY ROOT context because go.mod `replace`s
# the shared module at ../shared. Build with:
#   docker build -f browser-proxy/Dockerfile .
ARG GO_VERSION=1.26.8

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY shared/go.mod shared/go.sum ./shared/
COPY browser-proxy/go.mod browser-proxy/go.sum ./browser-proxy/
RUN cd browser-proxy && go mod download

COPY shared ./shared
COPY browser-proxy ./browser-proxy
RUN cd browser-proxy && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/browser-proxy ./cmd/browser-proxy

# Hardened minimal runtime (same distroless pattern as the tool gateway:
# no shell, read-only rootfs enforced at the pod securityContext).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/browser-proxy /usr/local/bin/browser-proxy
EXPOSE 8888
USER 65532:65532
ENTRYPOINT ["browser-proxy"]
