ARG GO_VERSION=1.26.8

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/skquad-api ./cmd/api

FROM alpine:3.20
RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 65532 skquad
COPY --from=build /out/skquad-api /usr/local/bin/skquad-api
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["skquad-api"]
