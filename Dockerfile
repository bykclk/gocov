FROM golang:1.26-alpine AS build
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION can be passed explicitly; otherwise it is derived from git so
# `gocov-server version` reports the checked-out tag instead of "dev".
ARG VERSION=
RUN ver="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}" && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${ver}" \
    -o /out/gocov-server ./cmd/gocov-server

FROM alpine:3.21
RUN adduser -D -H gocov
COPY --from=build /out/gocov-server /usr/local/bin/gocov-server
USER gocov
EXPOSE 8080
ENTRYPOINT ["gocov-server"]
CMD ["serve"]
