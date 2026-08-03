# ---- build ----
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# modernc.org/sqlite is pure Go, so we build a fully static binary (no cgo).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/news ./cmd/news

# ---- runtime ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=build /out/news /app/news

# Persist the SQLite DB on a mounted volume (e.g. Railway volume at /data).
ENV NEWS_DB=/data/news.db
# Refresh (ingest + enrich + re-theme) every 6 hours by default in a container.
ENV NEWS_REFRESH_MINUTES=360
VOLUME ["/data"]

# Railway/most PaaS inject $PORT; the app reads it. Default 8787 for local.
EXPOSE 8787
ENTRYPOINT ["/app/news"]
CMD ["serve"]
