FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cloudbox ./cmd/cloudbox

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 1000 cloudbox \
    && mkdir -p /data/blobs /data/tmp && chown -R cloudbox /data
COPY --from=build /out/cloudbox /usr/local/bin/cloudbox
USER cloudbox
ENV CB_LISTEN=:8080 CB_DATA_DIR=/data/blobs CB_TMP_DIR=/data/tmp
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["cloudbox"]
