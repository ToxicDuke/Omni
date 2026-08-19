# Stage 1 (Build)
FROM golang:1.24.11-alpine AS builder

ARG VERSION
RUN apk add --update --no-cache git make mailcap
WORKDIR /app/
COPY go.mod go.sum /app/
RUN go mod download
COPY . /app/
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/pterodactyl/wings/system.Version=$VERSION" \
    -v \
    -trimpath \
    -o omni \
    wings.go
RUN echo "ID=\"distroless\"" > /etc/os-release

# Stage 2 (Final)
FROM gcr.io/distroless/static:latest
COPY --from=builder /etc/os-release /etc/os-release
COPY --from=builder /etc/mime.types /etc/mime.types

COPY --from=builder /app/omni /usr/bin/

ENTRYPOINT ["/usr/bin/omni"]
CMD ["--config", "/etc/pterodactyl/config.yml"]

EXPOSE 8080 2022
