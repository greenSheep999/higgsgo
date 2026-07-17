# syntax=docker/dockerfile:1.7

# ---- build stage ----
FROM golang:1.22-alpine AS build

WORKDIR /src

# Cache module downloads.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
ENV CGO_ENABLED=0

RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/higgsgo ./cmd/higgsgo \
 && GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
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
