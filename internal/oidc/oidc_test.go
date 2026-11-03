package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Фейковый провайдер: discovery, JWKS, token endpoint с проверкой PKCE.
func TestFlow(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var srv *httptest.Server
	var gotVerifier, challenge, nonce string
	sign := func(claims map[string]any) string {
		h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1", "typ": "JWT"})
		p, _ := json.Marshal(claims)
		in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p)
		d := sha256.Sum256([]byte(in))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
		return in + "." + base64.RawURLEncoding.EncodeToString(sig)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "authorization_endpoint": srv.URL + "/auth",
			"token_endpoint": srv.URL + "/token", "jwks_uri": srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": "k1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		gotVerifier = r.Form.Get("code_verifier")
		sum := sha256.Sum256([]byte(gotVerifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge || r.Form.Get("code") != "the-code" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id_token": sign(map[string]any{
			"iss": srv.URL, "aud": "cloudbox", "sub": "u-42", "preferred_username": "andrey",
			"email": "a@example.ru", "nonce": nonce, "exp": time.Now().Add(time.Hour).Unix(),
		})})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	p := New(Config{Issuer: srv.URL, ClientID: "cloudbox", ClientSecret: "s", RedirectURL: "https://cloud/cb"})
	ctx := context.Background()
	nonce = "n-123"
	u, err := p.AuthURL(ctx, "st", nonce, "verifier-abc")
	if err != nil {
		t.Fatal(err)
	}
	pu, _ := url.Parse(u)
	challenge = pu.Query().Get("code_challenge")
	if pu.Query().Get("code_challenge_method") != "S256" || pu.Query().Get("state") != "st" {
		t.Fatalf("auth url: %s", u)
	}
	c, err := p.Exchange(ctx, "the-code", "verifier-abc", nonce)
	if err != nil {
		t.Fatal(err)
	}
	if c.Sub != "u-42" || c.PreferredUsername != "andrey" {
		t.Fatalf("claims: %+v", c)
	}
	if _, err := p.Exchange(ctx, "the-code", "verifier-abc", "other-nonce"); err == nil {
		t.Fatal("nonce mismatch accepted")
	}
	if _, err := p.Exchange(ctx, "the-code", "wrong-verifier", nonce); err == nil {
		t.Fatal("PKCE mismatch accepted")
	}
	// подделка подписи
	tok := sign(map[string]any{"iss": srv.URL, "aud": "cloudbox", "sub": "x", "exp": time.Now().Add(time.Hour).Unix()})
	parts := strings.Split(tok, ".")
	evil, _ := json.Marshal(map[string]any{"iss": srv.URL, "aud": "cloudbox", "sub": "admin", "exp": time.Now().Add(time.Hour).Unix()})
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString(evil) + "." + parts[2]
	if _, err := p.Verify(ctx, forged); err == nil {
		t.Fatal("forged token accepted")
	}
	// чужой aud и просроченный
	if _, err := p.Verify(ctx, sign(map[string]any{"iss": srv.URL, "aud": "other", "sub": "x", "exp": time.Now().Add(time.Hour).Unix()})); err == nil {
		t.Fatal("foreign aud accepted")
	}
	if _, err := p.Verify(ctx, sign(map[string]any{"iss": srv.URL, "aud": "cloudbox", "sub": "x", "exp": time.Now().Add(-time.Hour).Unix()})); err == nil {
		t.Fatal("expired accepted")
	}
	// alg=none
	none := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"k1"}`)) + "." + parts[1] + "."
	if _, err := p.Verify(ctx, none); err == nil {
		t.Fatal("alg none accepted")
	}
}
