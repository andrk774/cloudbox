// Package mail — веб-клиент почты: ящики пользователя на внешних IMAP/SMTP-серверах.
//
// CloudBox не принимает почту сам: он подключается к существующему ящику (Яндекс, Mail.ru,
// свой сервер) и показывает письма в веб-интерфейсе — как Nextcloud Mail.
package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

var (
	ErrBlockedHost = errors.New("подключаться можно только к публичным почтовым серверам (свой сервер в локальной сети — CB_MAIL_ALLOW_PRIVATE=true)")
	ErrBadPort     = errors.New("недопустимый порт: разрешены 143, 993 (IMAP), 25, 465, 587, 2525 (SMTP); другие администратор добавляет в CB_MAIL_EXTRA_PORTS")
	ErrPlaintext   = errors.New("соединение без шифрования разрешено только с сервером в локальной сети при CB_MAIL_ALLOW_PRIVATE=true")
)

// Порты, на которые разрешено подключаться: адрес и порт задаёт пользователь, и без ограничения
// форма «добавить ящик» превратилась бы в сканер портов внутренней сети сервера (SSRF).
var allowedPorts = []int{143, 993, 25, 465, 587, 2525}

// Security — способ защиты соединения.
const (
	SecTLS      = "tls"      // сразу TLS (993, 465)
	SecSTARTTLS = "starttls" // обычное соединение, затем STARTTLS (143, 587)
	SecNone     = "none"     // без шифрования — только для сервера в локальной сети
)

// Dialer подключается к почтовым серверам с проверкой адреса.
type Dialer struct {
	AllowPrivate bool          // разрешить частные адреса (свой сервер в локальной сети)
	Timeout      time.Duration // на установку соединения
	Resolver     *net.Resolver
	// ExtraPorts — разрешённые сверх стандартных (CB_MAIL_EXTRA_PORTS), например 1143,1025 у Proton Mail Bridge
	ExtraPorts []int
}

func (d *Dialer) timeout() time.Duration {
	if d.Timeout > 0 {
		return d.Timeout
	}
	return 15 * time.Second
}

// blocked — адреса, к которым нельзя подключаться без AllowPrivate.
func blocked(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		// 100.64.0.0/10 — CGNAT, часто внутренние сети провайдеров и облаков
		(ip.To4() != nil && ip.To4()[0] == 100 && ip.To4()[1]&0xc0 == 64) ||
		// 0.0.0.0/8
		(ip.To4() != nil && ip.To4()[0] == 0)
}

// CheckSecurity — допустим ли выбранный способ защиты.
func (d *Dialer) CheckSecurity(sec string) error {
	switch sec {
	case SecTLS, SecSTARTTLS:
		return nil
	case SecNone:
		if d.AllowPrivate {
			return nil // окончательно проверяется в Dial: адрес должен оказаться частным
		}
		return ErrPlaintext
	}
	return fmt.Errorf("неизвестный способ защиты %q", sec)
}

// Dial проверяет адрес и подключается именно к проверенному IP (без повторного DNS-запроса,
// чтобы ответ DNS нельзя было подменить между проверкой и подключением).
// При sec == SecTLS возвращает уже установленное TLS-соединение.
func (d *Dialer) Dial(ctx context.Context, host string, port int, sec string) (net.Conn, error) {
	if !slices.Contains(allowedPorts, port) && !slices.Contains(d.ExtraPorts, port) {
		return nil, ErrBadPort
	}
	if err := d.CheckSecurity(sec); err != nil {
		return nil, err
	}
	host = strings.TrimSuffix(strings.TrimSpace(host), ".")
	if host == "" || strings.ContainsAny(host, "/ \t") {
		return nil, errors.New("неверный адрес сервера")
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout())
	defer cancel()
	var ips []net.IP
	if ip := net.ParseIP(host); ip != nil {
		ips = []net.IP{ip}
	} else {
		r := d.Resolver
		if r == nil {
			r = net.DefaultResolver
		}
		addrs, err := r.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("сервер %s не найден", host)
		}
		for _, a := range addrs {
			ips = append(ips, a.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("сервер %s не найден", host)
	}
	var lastErr error
	for _, ip := range ips {
		priv := blocked(ip)
		if priv && !d.AllowPrivate {
			lastErr = ErrBlockedHost
			continue
		}
		if sec == SecNone && !priv {
			lastErr = ErrPlaintext // пароль открытым текстом через интернет не отправляем
			continue
		}
		var nd net.Dialer
		conn, err := nd.DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		if err != nil {
			lastErr = fmt.Errorf("не удалось подключиться к %s:%d", host, port)
			continue
		}
		if sec == SecTLS {
			tc := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
			if err := tc.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, fmt.Errorf("TLS с %s: %v", host, err)
			}
			return tc, nil
		}
		return conn, nil
	}
	return nil, lastErr
}

// TLSConfig — для STARTTLS: проверка сертификата по имени сервера.
func TLSConfig(host string) *tls.Config {
	return &tls.Config{ServerName: strings.TrimSuffix(host, "."), MinVersion: tls.VersionTLS12}
}
