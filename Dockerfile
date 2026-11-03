FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cloudbox ./cmd/cloudbox \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cloudbox-sync ./cmd/cloudbox-sync

FROM alpine:3.22
# ffmpeg — миниатюры видео, poppler-utils — миниатюры и поиск по PDF, postgresql-client — бэкапы
RUN apk add --no-cache ca-certificates tzdata ffmpeg poppler-utils postgresql16-client \
 && adduser -D -H -u 1000 cloudbox \
 && mkdir -p /data/blobs /data/tmp /backups && chown -R cloudbox /data /backups
COPY --from=build /out/cloudbox /out/cloudbox-sync /usr/local/bin/
USER cloudbox
ENV CB_LISTEN=:8080 CB_DATA_DIR=/data/blobs CB_TMP_DIR=/data/tmp TZ=Asia/Yekaterinburg
VOLUME ["/data", "/backups"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["cloudbox"]
