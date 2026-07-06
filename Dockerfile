FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/gocov-server ./cmd/gocov-server

FROM alpine:3.21
RUN adduser -D -H gocov
COPY --from=build /out/gocov-server /usr/local/bin/gocov-server
USER gocov
EXPOSE 8080
ENTRYPOINT ["gocov-server"]
CMD ["serve"]
