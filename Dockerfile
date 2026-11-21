# ---- Build stage ----
FROM golang:1.26-alpine AS build
ARG GOPROXY
ENV GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}
ARG GO_BUILD_PARALLEL=2
ENV GOFLAGS=-p=${GO_BUILD_PARALLEL}
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=overmindv-gomod,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,id=overmindv-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=overmindv-gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/media ./cmd/media-renderer ./cmd/media-reconcile

FROM alpine:3.22 AS runtime
RUN apk upgrade --no-cache \
    && addgroup -S -g 10003 media-io

FROM runtime AS renderer
RUN apk add --no-cache vips-tools \
    && adduser -S -D -H -u 10002 -G media-io media-renderer \
    && mkdir -p /run/media-renderer \
    && chown 10002:10003 /run/media-renderer \
    && chmod 0750 /run/media-renderer
COPY --from=build /out/media-renderer /usr/local/bin/media-renderer
USER 10002:10003
ENTRYPOINT ["media-renderer"]

FROM runtime AS runner
RUN apk add --no-cache ca-certificates wget \
    && adduser -S -D -H -u 10001 -G media-io media
WORKDIR /app
COPY --from=build /out/media /usr/local/bin/media
COPY --from=build /out/media-reconcile /usr/local/bin/media-reconcile
COPY migrations /app/migrations
USER 10001:10003
EXPOSE 8080
ENTRYPOINT ["media"]
