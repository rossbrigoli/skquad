# skquad-build-service (MIG-8): governed agent build path.
# Build from THIS directory's context (matches images.yml):
#   docker build -t skquad-build-service build-service
#
# Runtime needs `git` (revision resolution: ls-remote + depth-1 fetch).
# The binary itself is static; the runtime layer is minimal alpine.
ARG GO_VERSION=1.26.10

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-build-service ./cmd/build-service

FROM alpine:3.22
RUN apk add --no-cache git ca-certificates
COPY --from=build /out/skquad-build-service /usr/local/bin/skquad-build-service
EXPOSE 8090
USER 65532:65532
ENTRYPOINT ["skquad-build-service"]
