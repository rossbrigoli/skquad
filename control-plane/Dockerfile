# TG-3: builds from the REPOSITORY ROOT context because go.mod `replace`s
# the shared module at ../shared. Build with:
#   docker build -f control-plane/Dockerfile .
ARG GO_VERSION=1.26.8

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY shared/go.mod shared/go.sum ./shared/
COPY control-plane/go.mod control-plane/go.sum ./control-plane/
RUN cd control-plane && go mod download

COPY shared ./shared
COPY control-plane ./control-plane
RUN cd control-plane \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-api ./cmd/api \
 && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/embed-backfill ./cmd/embed-backfill

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 65532 skquad
COPY --from=build /out/skquad-api /usr/local/bin/skquad-api
# S-212: ship the backfill binary too so a model change can re-embed
# existing memories without a separate image (used by the Helm
# post-upgrade hook Job, or manually via kubectl exec).
COPY --from=build /out/embed-backfill /usr/local/bin/embed-backfill
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["skquad-api"]
