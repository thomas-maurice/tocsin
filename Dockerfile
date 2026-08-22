# Stage 1: admin UI (embedded into the Go binary).
FROM node:22 AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --no-fund --no-audit
COPY ui/ .
RUN npm run build

# Stage 2: Go binary. CGO is required by the SQLite crypto store, so each
# target platform compiles natively (buildx + qemu for cross-arch).
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist ui/dist
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -tags goolm \
    -ldflags "-s -w -X github.com/thomas-maurice/tocsin/internal/api.Version=${VERSION}" \
    -o /tocsin ./cmd/tocsin

# Stage 3: runtime.
FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 1000 --create-home --home-dir /data notifier
COPY --from=build /tocsin /usr/local/bin/tocsin
USER notifier
WORKDIR /data
VOLUME /data
EXPOSE 8686
ENTRYPOINT ["/usr/local/bin/tocsin"]
CMD ["--config", "/config/config.yaml"]
