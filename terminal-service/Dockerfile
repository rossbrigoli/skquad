# TG-10: terminal-service (Terminal-as-a-Service). Build from THIS directory's
# context (matches images.yml `context: terminal-service`); the module is
# self-contained (no replace of ../shared):
#   docker build -t skquad-terminal-service terminal-service
ARG GO_VERSION=1.26.8

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-terminal-service ./cmd/terminal

# Hardened minimal runtime: no shell, no SSH client binaries — the service
# speaks SSH via its compiled Go library only. Read-only rootfs enforced
# at the pod securityContext (chart).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/skquad-terminal-service /usr/local/bin/skquad-terminal-service
EXPOSE 8091
USER 65532:65532
ENTRYPOINT ["skquad-terminal-service"]
