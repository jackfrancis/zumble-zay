# Standard Dockerfile (no BuildKit-only features) so it builds identically with
# `docker build` and `podman build`.

# ---- build stage ----
FROM golang:1.25 AS build
WORKDIR /src

# Cache dependencies first.
COPY go.mod go.sum ./
RUN go mod download

# Build a static, stripped binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

# ---- runtime stage ----
# distroless static: no shell, non-root by default, ships CA certs for the
# outbound HTTPS calls to the OAuth providers.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
