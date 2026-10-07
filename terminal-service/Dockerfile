# TG-10: terminal-service (Terminal-as-a-Service). Build from repo root:
#   docker build -f terminal-service/Dockerfile .
ARG GO_VERSION=1.26.8

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY terminal-service/go.mod terminal-service/go.sum ./terminal-service/
RUN cd terminal-service && go mod download

COPY terminal-service ./terminal-service
RUN cd terminal-service && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-terminal-service ./cmd/terminal

# Hardened minimal runtime: no shell, no SSH client binaries — the service
# speaks SSH via its compiled Go library only. Read-only rootfs enforced
# at the pod securityContext (chart).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/skquad-terminal-service /usr/local/bin/skquad-terminal-service
EXPOSE 8091
USER 65532:65532
ENTRYPOINT ["skquad-terminal-service"]
