# syntax=docker/dockerfile:1

# Build stage: full Go toolchain, discarded from the final image.
FROM golang:1.27-alpine AS build
WORKDIR /src
# Cache modules separately from sources so edits don't refetch dependencies.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static binary: no libc/toolchain needed at runtime.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/herald ./cmd/herald

# Runtime stage: small, non-root, no build toolchain.
FROM alpine:3.21
# ca-certificates is required to verify TLS for SMTP STARTTLS and webhook sinks.
RUN apk add --no-cache ca-certificates \
	&& addgroup -S herald \
	&& adduser -S -G herald -H -h /nonexistent herald
COPY --from=build /out/herald /usr/local/bin/herald
USER herald
EXPOSE 8787
# The healthcheck in docker-compose.yml hits GET /healthz (liveness); /readyz is
# the readiness endpoint. Neither is defined here so non-Compose users can pick.
ENTRYPOINT ["herald"]
CMD ["serve", "--config", "/etc/herald/gateway.toml"]
