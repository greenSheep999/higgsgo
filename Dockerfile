# syntax=docker/dockerfile:1.7

# ---- build stage ----
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build-time metadata. VERSION is the semver tag (from the CI ref name or a
# manual --build-arg); COMMIT is the short git sha; BUILD_TIME is ISO-8601 UTC.
# All three are injected into internal/version at link time so /admin/version
# and the sidebar footer can display a real build ID.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_TIME=unknown
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0

RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/greensheep999/higgsgo/internal/version.Version=${VERSION} \
        -X github.com/greensheep999/higgsgo/internal/version.Commit=${COMMIT} \
        -X github.com/greensheep999/higgsgo/internal/version.BuildTime=${BUILD_TIME}" \
      -o /out/higgsgo ./cmd/higgsgo \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath \
      -ldflags "-s -w \
        -X github.com/greensheep999/higgsgo/internal/version.Version=${VERSION} \
        -X github.com/greensheep999/higgsgo/internal/version.Commit=${COMMIT} \
        -X github.com/greensheep999/higgsgo/internal/version.BuildTime=${BUILD_TIME}" \
      -o /out/higgsgo-cli ./cmd/higgsgo-cli

# ---- runtime stage ----
FROM gcr.io/distroless/base-debian12:nonroot

WORKDIR /app
COPY --from=build /out/higgsgo /app/higgsgo
COPY --from=build /out/higgsgo-cli /app/higgsgo-cli

# Static data + example config so the container has a runnable default.
COPY data /app/data
COPY configs /app/configs

# 18080 = public /v1
# 18081 = admin /admin
# 18082 = internal /internal (CPA plugin)
EXPOSE 18080 18081 18082

VOLUME ["/app/data"]

ENTRYPOINT ["/app/higgsgo"]
CMD ["-config", "/app/configs/higgsgo.example.toml"]
