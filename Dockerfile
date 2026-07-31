FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/komari-bridge ./cmd/komari-bridge

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && addgroup -S bridge && adduser -S -G bridge bridge
USER bridge
WORKDIR /app
COPY --from=build /out/komari-bridge /usr/local/bin/komari-bridge
VOLUME ["/app/data"]
EXPOSE 9090
ENTRYPOINT ["komari-bridge"]
CMD ["-config", "/app/config.yaml"]
