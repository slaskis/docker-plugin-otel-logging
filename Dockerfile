
FROM golang:1.23-alpine AS builder

WORKDIR /plugin

COPY go.* .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -a -ldflags '-extldflags "-static"' -o docker-plugin-otel-logging .

FROM scratch

COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group
COPY --from=builder /tmp /tmp
COPY --from=builder /run /run

# Copy only the binary
COPY --from=builder /plugin/docker-plugin-otel-logging /usr/bin/

# Set the entrypoint
ENTRYPOINT ["/usr/bin/docker-plugin-otel-logging"]
