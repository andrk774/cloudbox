// Package netx — адрес клиента за обратным прокси и ограничение частоты попыток.
package netx

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTrustedProxies — loopback и частные сети: обычный случай nginx/Caddy на той же машине или в docker-сети.
const DefaultTrustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"

var trusted atomic.Pointer[[]*net.IPNet]

func init() { _ = SetTrustedProxies(DefaultTrustedProxies) }

// SetTrustedProxies задаёт сети, от которых принимаются X-Real-IP / X-Forwarded-For.
// Пустая строка или "none" — не доверять никому (сервер смотрит в интернет напрямую).
func SetTrustedProxies(spec string) error {
	var nets []*net.IPNet
	spec = strings.TrimSpace(spec)
	if spec != "" && !strings.EqualFold(spec, "none") {
		for _, part := range strings.Split(spec, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !strings.Contains(part, "/") {
				if ip := net.ParseIP(part); ip != nil && ip.To4() != nil {
					part += "/32"
				} else {
					part += "/128"
				}
			}
			_, n, err := net.ParseCIDR(part)
			if err != nil {
				return err
			}
			nets = append(nets, n)
		}
	}
	trusted.Store(&nets)
	return nil
}

func isTrusted(ip net.IP) bool {
	if ip == nil {
		return false
	}
	for _, n := range *trusted.Load() {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ClientIP — адрес клиента. Заголовки прокси учитываются, только если соединение пришло от доверенного прокси;
// в X-Forwarded-For берётся самый правый недоверенный адрес (левые части клиент может подставить сам).
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !isTrusted(net.ParseIP(host)) {
		return host
	}
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" && net.ParseIP(xr) != nil {
		return xr
	}
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		parts := strings.Split(xf, ",")
		for i := len(parts) - 1; i >= 0; i-- {
			p := strings.TrimSpace(parts[i])
			ip := net.ParseIP(p)
			if ip == nil {
				break
			}
			if !isTrusted(ip) || i == 0 {
				return p
			}
		}
	}
	return host
}

// Limiter — счётчики попыток в фиксированном окне. Просроченные записи вычищаются, память не растёт бесконечно.
type Limiter struct {
	mu sync.Mutex
	m  map[string]*bucket
}

type bucket struct {
	n     int
	reset time.Time
}

func (l *Limiter) get(key string, window time.Duration, now time.Time) *bucket {
	if l.m == nil {
		l.m = map[string]*bucket{}
	}
	if len(l.m) > 50000 {
		for k, b := range l.m {
			if now.After(b.reset) {
				delete(l.m, k)
			}
		}
		if len(l.m) > 100000 { // массовая атака с множества адресов — сбрасываем, лишь бы не съесть память
			l.m = map[string]*bucket{}
		}
	}
	b, ok := l.m[key]
	if !ok || now.After(b.reset) {
		b = &bucket{reset: now.Add(window)}
		l.m[key] = b
	}
	return b
}

// Allow засчитывает попытку и сообщает, укладывается ли она в max за окно.
func (l *Limiter) Allow(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(key, window, time.Now())
	b.n++
	return b.n <= max
}

// Blocked — исчерпан ли лимит (без учёта новой попытки).
func (l *Limiter) Blocked(key string, max int, window time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.get(key, window, time.Now()).n >= max
}

// Fail засчитывает неудачную попытку.
func (l *Limiter) Fail(key string, window time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.get(key, window, time.Now()).n++
}
