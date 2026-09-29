# termhub Hub image built entirely inside Docker (docs/M8 第 7 节), for hosts
# that can reach the public registries. bash deploy/release.sh with
# Dockerfile.prebuilt is the path used in practice; this one is an alternative.
#
# Build from the repository root:
#   docker build -f deploy/Dockerfile --build-arg VERSION=0.1.0-local -t localhost/termhub:latest .

FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# the build writes into ../internal/hub/web/dist, embedded by the Go build below
COPY internal/hub/web/ /src/internal/hub/web/
ARG VERSION=dev
RUN TH_WEB_VERSION=${VERSION} npm run build

FROM golang:1.27-alpine AS build
WORKDIR /src
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/hub/web/dist/ internal/hub/web/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/termhub ./cmd/termhub
# The data directory must exist and belong to the unprivileged user before the
# volume is mounted over it.
RUN mkdir -p /out/data

# No shell, no package manager: only the program, CA certificates and time zones.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/termhub /termhub
COPY --from=build --chown=nonroot:nonroot /out/data /data
ENV TH_DATA_DIR=/data
VOLUME /data
EXPOSE 27443 27180
USER nonroot
ENTRYPOINT ["/termhub"]
