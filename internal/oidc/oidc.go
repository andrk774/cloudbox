// Package oidc — вход через OpenID Connect (Authentik, Keycloak, Authelia, Zitadel, Яндекс ID с OIDC и т.п.).
// Authorization Code + PKCE, проверка подписи ID-токена по JWKS (RS256/RS384/RS512, ES256/ES384).
package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
}

type Provider struct {
	cfg  Config
	http *http.Client

	mu       sync.Mutex
	meta     *metadata
	keys     map[string]crypto.PublicKey
	keysTime time.Time
}

type metadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

func New(cfg Config) *Provider {
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "email"}
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	return &Provider{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}}
}

func (p *Provider) discover(ctx context.Context) (*metadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meta != nil {
		return p.meta, nil
	}
	var m metadata
	if err := p.getJSON(ctx, p.cfg.Issuer+"/.well-known/openid-configuration", &m); err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	if strings.TrimRight(m.Issuer, "/") != p.cfg.Issuer {
		return nil, fmt.Errorf("oidc: issuer в метаданных %q не совпадает с CB_OIDC_ISSUER", m.Issuer)
	}
	p.meta = &m
	return p.meta, nil
}

func (p *Provider) getJSON(ctx context.Context, u string, v any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(v)
}

// AuthURL — адрес, куда отправить пользователя. state, nonce и verifier клиент хранит в cookie.
func (p *Provider) AuthURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	ch := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.cfg.ClientID},
		"redirect_uri":          {p.cfg.RedirectURL},
		"scope":                 {strings.Join(p.cfg.Scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(ch[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(m.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return m.AuthorizationEndpoint + sep + q.Encode(), nil
}

// Claims — нужные поля ID-токена.
type Claims struct {
	Sub               string `json:"sub"`
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	Nonce             string `json:"nonce"`
	Iss               string `json:"iss"`
	Aud               any    `json:"aud"`
	Exp               int64  `json:"exp"`
	Iat               int64  `json:"iat"`
}

// Exchange меняет code на токены и возвращает проверенные claims ID-токена.
func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (*Claims, error) {
	m, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"client_id":     {p.cfg.ClientID},
		"code_verifier": {verifier},
	}
	if p.cfg.ClientSecret != "" {
		form.Set("client_secret", p.cfg.ClientSecret)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, m.TokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		IDToken string `json:"id_token"`
		Error   string `json:"error"`
		Desc    string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil {
		return nil, fmt.Errorf("oidc token: %w", err)
	}
	if resp.StatusCode != 200 || tok.IDToken == "" {
		return nil, fmt.Errorf("oidc token: HTTP %d %s %s", resp.StatusCode, tok.Error, tok.Desc)
	}
	c, err := p.Verify(ctx, tok.IDToken)
	if err != nil {
		return nil, err
	}
	if c.Nonce != nonce {
		return nil, errors.New("oidc: nonce не совпадает")
	}
	return c, nil
}

// Verify проверяет подпись и стандартные поля ID-токена.
func (p *Provider) Verify(ctx context.Context, raw string) (*Claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("oidc: неверный формат токена")
	}
	hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	pb, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, errors.New("oidc: неверная кодировка токена")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, err
	}
	key, err := p.key(ctx, hdr.Kid)
	if err != nil {
		return nil, err
	}
	if err := verifySig(hdr.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if strings.TrimRight(c.Iss, "/") != p.cfg.Issuer {
		return nil, errors.New("oidc: чужой issuer")
	}
	if !audContains(c.Aud, p.cfg.ClientID) {
		return nil, errors.New("oidc: токен выдан не для этого клиента")
	}
	if c.Exp < now-60 {
		return nil, errors.New("oidc: токен просрочен")
	}
	if c.Sub == "" {
		return nil, errors.New("oidc: нет sub")
	}
	return &c, nil
}

func audContains(aud any, id string) bool {
	switch a := aud.(type) {
	case string:
		return a == id
	case []any:
		for _, x := range a {
			if s, ok := x.(string); ok && s == id {
				return true
			}
		}
	}
	return false
}

func (p *Provider) key(ctx context.Context, kid string) (crypto.PublicKey, error) {
	p.mu.Lock()
	k, ok := p.keys[kid]
	fresh := time.Since(p.keysTime) < time.Hour
	p.mu.Unlock()
	if ok && fresh {
		return k, nil
	}
	if err := p.loadKeys(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if k, ok := p.keys[kid]; ok {
		return k, nil
	}
	if kid == "" && len(p.keys) == 1 {
		for _, k := range p.keys {
			return k, nil
		}
	}
	return nil, errors.New("oidc: ключ подписи не найден в JWKS")
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (p *Provider) loadKeys(ctx context.Context) error {
	m, err := p.discover(ctx)
	if err != nil {
		return err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := p.getJSON(ctx, m.JWKSURI, &set); err != nil {
		return err
	}
	keys := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pk, err := ParseJWK(k.Kty, k.N, k.E, k.Crv, k.X, k.Y)
		if err == nil {
			keys[k.Kid] = pk
		}
	}
	p.mu.Lock()
	p.keys, p.keysTime = keys, time.Now()
	p.mu.Unlock()
	return nil
}

// ParseJWK собирает публичный ключ из полей JWK.
func ParseJWK(kty, n, e, crv, x, y string) (crypto.PublicKey, error) {
	dec := base64.RawURLEncoding.DecodeString
	switch kty {
	case "RSA":
		nb, err1 := dec(n)
		eb, err2 := dec(e)
		if err1 != nil || err2 != nil {
			return nil, errors.New("bad rsa jwk")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}, nil
	case "EC":
		var c elliptic.Curve
		switch crv {
		case "P-256":
			c = elliptic.P256()
		case "P-384":
			c = elliptic.P384()
		default:
			return nil, errors.New("unsupported curve")
		}
		xb, err1 := dec(x)
		yb, err2 := dec(y)
		if err1 != nil || err2 != nil {
			return nil, errors.New("bad ec jwk")
		}
		return &ecdsa.PublicKey{Curve: c, X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}, nil
	}
	return nil, errors.New("unsupported key type")
}

func verifySig(alg string, key crypto.PublicKey, signed, sig []byte) error {
	var h hash.Hash
	var ch crypto.Hash
	switch alg {
	case "RS256", "ES256":
		h, ch = sha256.New(), crypto.SHA256
	case "RS384", "ES384":
		h, ch = sha512.New384(), crypto.SHA384
	case "RS512":
		h, ch = sha512.New(), crypto.SHA512
	default:
		return fmt.Errorf("oidc: алгоритм %q не поддерживается", alg)
	}
	h.Write(signed)
	digest := h.Sum(nil)
	switch k := key.(type) {
	case *rsa.PublicKey:
		if !strings.HasPrefix(alg, "RS") {
			return errors.New("oidc: алгоритм не соответствует ключу")
		}
		if err := rsa.VerifyPKCS1v15(k, ch, digest, sig); err != nil {
			return errors.New("oidc: неверная подпись")
		}
	case *ecdsa.PublicKey:
		if !strings.HasPrefix(alg, "ES") {
			return errors.New("oidc: алгоритм не соответствует ключу")
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return errors.New("oidc: неверная подпись")
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(k, digest, r, s) {
			return errors.New("oidc: неверная подпись")
		}
	default:
		return errors.New("oidc: неизвестный тип ключа")
	}
	return nil
}
