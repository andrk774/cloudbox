package netx

import (
	"net/http"
	"testing"
	"time"
)

func req(remote string, hdr map[string]string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	t.Cleanup(func() { SetTrustedProxies(DefaultTrustedProxies) })
	if err := SetTrustedProxies(DefaultTrustedProxies); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		remote string
		hdr    map[string]string
		want   string
	}{
		{"прямое подключение: заголовки игнорируются", "203.0.113.5:1234", map[string]string{"X-Real-IP": "1.1.1.1"}, "203.0.113.5"},
		{"прокси с X-Real-IP", "172.18.0.2:1234", map[string]string{"X-Real-IP": "198.51.100.7"}, "198.51.100.7"},
		{"подделанный левый XFF не принимается", "127.0.0.1:1", map[string]string{"X-Forwarded-For": "1.1.1.1, 198.51.100.7"}, "198.51.100.7"},
		{"цепочка прокси в XFF", "127.0.0.1:1", map[string]string{"X-Forwarded-For": "198.51.100.7, 10.0.0.3"}, "198.51.100.7"},
		{"мусор в X-Real-IP", "127.0.0.1:1", map[string]string{"X-Real-IP": "evil"}, "127.0.0.1"},
		{"без заголовков", "10.1.1.1:5", nil, "10.1.1.1"},
	}
	for _, c := range cases {
		if got := ClientIP(req(c.remote, c.hdr)); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}

	// "none" — не доверять никому, даже loopback
	if err := SetTrustedProxies("none"); err != nil {
		t.Fatal(err)
	}
	if got := ClientIP(req("127.0.0.1:1", map[string]string{"X-Real-IP": "1.1.1.1"})); got != "127.0.0.1" {
		t.Errorf("none: got %s", got)
	}
	// одиночный адрес без маски
	if err := SetTrustedProxies("192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	if got := ClientIP(req("192.0.2.10:1", map[string]string{"X-Real-IP": "198.51.100.1"})); got != "198.51.100.1" {
		t.Errorf("single ip: got %s", got)
	}
	if SetTrustedProxies("10.0.0.0/99") == nil {
		t.Error("bad cidr accepted")
	}
}

func TestLimiter(t *testing.T) {
	var l Limiter
	for i := 0; i < 3; i++ {
		if !l.Allow("k", 3, time.Minute) {
			t.Fatalf("attempt %d rejected", i+1)
		}
	}
	if l.Allow("k", 3, time.Minute) {
		t.Fatal("4th attempt allowed")
	}
	if l.Blocked("other", 1, time.Minute) {
		t.Fatal("fresh key blocked")
	}
	l.Fail("other", time.Minute)
	if !l.Blocked("other", 1, time.Minute) {
		t.Fatal("key not blocked after fail")
	}
	// окно истекло — счётчик сбрасывается
	l.Fail("short", time.Nanosecond)
	time.Sleep(time.Millisecond)
	if l.Blocked("short", 1, time.Minute) {
		t.Fatal("expired window still blocks")
	}
}
