# MIG-8: build service. Standalone Go module (no ../shared replace),
# builds with context build-service:
#   docker build -f build-service/Dockerfile build-service
ARG GO_VERSION=1.26.10

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-build-service ./cmd/buildsvc

# Hardened minimal runtime: no shell, no git binary (the service speaks
# smart-HTTP directly for ls-remote; kaniko does the actual builds in a
# separate pod). Read-only rootfs is enforced at the pod securityContext.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/skquad-build-service /usr/local/bin/skquad-build-service
EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["skquad-build-service"]
