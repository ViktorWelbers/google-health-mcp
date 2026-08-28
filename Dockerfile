# ---- build ----
FROM golang:1.27-alpine AS build

WORKDIR /src

# Cache dependencies separately from source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Fully static binary: no libc, no cgo, so it runs on scratch.
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/health-mcp .

# ---- runtime ----
FROM scratch

# TLS roots, needed to reach health.googleapis.com and oauth2.googleapis.com.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/health-mcp /health-mcp

# Non-root. Numeric because scratch has no /etc/passwd.
USER 65532:65532

ENV HEALTH_TOKEN_PATH=/var/run/health/token.json

EXPOSE 8080
ENTRYPOINT ["/health-mcp"]
CMD ["serve", "-http", ":8080"]
