// Package metrics — метрики в формате Prometheus без внешних зависимостей.
package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	UploadBytes   atomic.Int64
	DownloadBytes atomic.Int64
	started       = time.Now()

	mu       sync.Mutex
	requests = map[string]int64{} // "method|class" -> count
	durSum   = map[string]float64{}
)

// Observe учитывает HTTP-запрос. route — грубая группа (api, dav, caldav, public, static).
func Observe(route, method string, code int, d time.Duration) {
	key := route + "|" + method + "|" + fmt.Sprintf("%dxx", code/100)
	mu.Lock()
	requests[key]++
	durSum[route] += d.Seconds()
	mu.Unlock()
}

// Handler отдаёт метрики. Если token не пуст, нужен заголовок Authorization: Bearer <token>.
func Handler(db *sql.DB, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var sb strings.Builder
		sb.WriteString("# HELP cloudbox_http_requests_total HTTP-запросы\n# TYPE cloudbox_http_requests_total counter\n")
		mu.Lock()
		keys := make([]string, 0, len(requests))
		for k := range requests {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := strings.Split(k, "|")
			fmt.Fprintf(&sb, "cloudbox_http_requests_total{route=%q,method=%q,code=%q} %d\n", p[0], p[1], p[2], requests[k])
		}
		sb.WriteString("# TYPE cloudbox_http_request_seconds_sum counter\n")
		routes := make([]string, 0, len(durSum))
		for k := range durSum {
			routes = append(routes, k)
		}
		sort.Strings(routes)
		for _, k := range routes {
			fmt.Fprintf(&sb, "cloudbox_http_request_seconds_sum{route=%q} %.3f\n", k, durSum[k])
		}
		mu.Unlock()
		fmt.Fprintf(&sb, "# TYPE cloudbox_upload_bytes_total counter\ncloudbox_upload_bytes_total %d\n", UploadBytes.Load())
		fmt.Fprintf(&sb, "# TYPE cloudbox_download_bytes_total counter\ncloudbox_download_bytes_total %d\n", DownloadBytes.Load())
		fmt.Fprintf(&sb, "# TYPE cloudbox_uptime_seconds gauge\ncloudbox_uptime_seconds %.0f\n", time.Since(started).Seconds())

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		gauges := []struct{ name, help, q string }{
			{"cloudbox_users", "пользователи", `SELECT count(*) FROM users`},
			{"cloudbox_files", "файлы (без корзины)", `SELECT count(*) FROM nodes WHERE NOT is_dir AND deleted_at IS NULL`},
			{"cloudbox_folders", "папки", `SELECT count(*) FROM nodes WHERE is_dir AND deleted_at IS NULL`},
			{"cloudbox_trash_items", "объекты в корзинах", `SELECT count(*) FROM nodes WHERE trash_root`},
			{"cloudbox_storage_bytes", "объём всех версий", `SELECT COALESCE(sum(size),0) FROM versions`},
			{"cloudbox_blobs", "уникальные блобы", `SELECT count(DISTINCT blob_key) FROM versions`},
			{"cloudbox_shares_public", "публичные ссылки", `SELECT count(*) FROM shares`},
			{"cloudbox_shares_users", "шары пользователям", `SELECT count(*) FROM user_shares`},
			{"cloudbox_sessions", "активные сессии", `SELECT count(*) FROM sessions WHERE expires_at > now()`},
			{"cloudbox_index_queue", "очередь индексации", `SELECT count(*) FROM index_queue`},
			{"cloudbox_gc_queue", "очередь удаления блобов", `SELECT count(*) FROM blob_gc`},
		}
		for _, g := range gauges {
			var v int64
			if err := db.QueryRowContext(ctx, g.q).Scan(&v); err == nil {
				fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", g.name, g.help, g.name, g.name, v)
			}
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write([]byte(sb.String()))
	}
}
