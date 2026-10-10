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
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/media ./cmd/media

# ---- Run stage ----
# vips-tools нужен worker-у (vipsthumbnail) для создания WebP-вариантов изображений.
FROM alpine:3.22 AS runner
RUN apk add --no-cache ca-certificates wget vips-tools && addgroup -S media && adduser -S media -G media
WORKDIR /app
COPY --from=build /out/media /usr/local/bin/media
COPY migrations /app/migrations
USER media
EXPOSE 8080
ENTRYPOINT ["media"]
