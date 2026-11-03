package talk

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// ICEConfig — STUN/TURN-серверы для WebRTC.
//
// Без них звонок работает только когда участники в одной сети. TURN нужен, если хотя бы один
// участник за строгим NAT (мобильный интернет, корпоративная сеть) — он ретранслирует медиапоток.
type ICEConfig struct {
	STUN   []string // stun:host:3478
	TURN   []string // turn:host:3478?transport=udp, turns:host:5349
	Secret string   // static-auth-secret coturn (use-auth-secret)
	TTL    time.Duration
}

type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Servers — список для RTCPeerConnection. Учётные данные TURN временные (TURN REST API):
// username = "<срок>:<uid>", credential = base64(HMAC-SHA1(secret, username)). Пароль от TURN
// в браузер не попадает, а выданные данные перестают работать по истечении срока.
func (c ICEConfig) Servers(uid int64, now time.Time) []ICEServer {
	out := []ICEServer{}
	if len(c.STUN) > 0 {
		out = append(out, ICEServer{URLs: c.STUN})
	}
	if len(c.TURN) > 0 && c.Secret != "" {
		ttl := c.TTL
		if ttl <= 0 {
			ttl = 12 * time.Hour
		}
		user := strconv.FormatInt(now.Add(ttl).Unix(), 10) + ":" + strconv.FormatInt(uid, 10)
		m := hmac.New(sha1.New, []byte(c.Secret))
		m.Write([]byte(user))
		out = append(out, ICEServer{URLs: c.TURN, Username: user, Credential: base64.StdEncoding.EncodeToString(m.Sum(nil))})
	}
	return out
}
