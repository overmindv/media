FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/media-api ./cmd/media-api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/media-worker ./cmd/media-worker
RUN GOBIN=/out go install -tags="no_clickhouse no_mssql no_mysql no_sqlite3 no_libsql no_ydb no_vertica" github.com/pressly/goose/v3/cmd/goose@v3.26.0

FROM alpine:3.22 AS api
RUN apk add --no-cache ca-certificates wget && addgroup -S media && adduser -S media -G media
WORKDIR /app
COPY --from=build /out/media-api /usr/local/bin/media-api
COPY --from=build /out/goose /usr/local/bin/goose
COPY migrations /app/migrations
USER media
EXPOSE 8080
ENTRYPOINT ["media-api"]

FROM alpine:3.22 AS worker
RUN apk add --no-cache ca-certificates vips-tools wget && addgroup -S media && adduser -S media -G media
WORKDIR /app
COPY --from=build /out/media-worker /usr/local/bin/media-worker
USER media
EXPOSE 8081
ENTRYPOINT ["media-worker"]
