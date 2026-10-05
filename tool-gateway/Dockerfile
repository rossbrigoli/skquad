# TG-3: builds from the REPOSITORY ROOT context because go.mod `replace`s
# the shared module at ../shared. Build with:
#   docker build -f tool-gateway/Dockerfile .
ARG GO_VERSION=1.26.8

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY shared/go.mod shared/go.sum ./shared/
COPY tool-gateway/go.mod tool-gateway/go.sum ./tool-gateway/
RUN cd tool-gateway && go mod download

COPY shared ./shared
COPY tool-gateway ./tool-gateway
RUN cd tool-gateway && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-tool-gateway ./cmd/gateway

# Hardened minimal runtime (design §5.2: minimal image, no shell/exec tools,
# read-only rootfs enforced at the pod securityContext).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/skquad-tool-gateway /usr/local/bin/skquad-tool-gateway
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["skquad-tool-gateway"]
