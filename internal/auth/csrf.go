package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const csrfCookie = "agent_transcripts_csrf"

// CSRF uses a signed synchronizer token: a readable nonce stored in a Secure
// cookie and an HMAC-bound token submitted in the form/header.
type CSRF struct {
	key    []byte
	origin *url.URL
	now    func() time.Time
}

func NewCSRF(key []byte, externalOrigin string) (*CSRF, error) {
	u, err := url.Parse(externalOrigin)
	if err != nil || u.Scheme == "" || u.Host == "" || len(key) < 32 {
		return nil, errors.New("invalid CSRF configuration")
	}
	return &CSRF{key: append([]byte(nil), key...), origin: u, now: time.Now}, nil
}
func NewLocalCSRF(key []byte) (*CSRF, error) {
	if len(key) < 32 {
		return nil, errors.New("invalid CSRF configuration")
	}
	return &CSRF{key: append([]byte(nil), key...), now: time.Now}, nil
}
func (c *CSRF) Token(w http.ResponseWriter, r *http.Request) string {
	if cookie, err := r.Cookie(csrfCookie); err == nil && c.validCookie(cookie.Value) {
		return c.sign(cookie.Value)
	}
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	nonce := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: nonce, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int((2 * time.Hour).Seconds())})
	return c.sign(nonce)
}
func (c *CSRF) validCookie(v string) bool {
	_, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil
}
func (c *CSRF) sign(nonce string) string {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
func (c *CSRF) Check(r *http.Request) bool {
	if !sameOrigin(r, c.origin) {
		return false
	}
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || !c.validCookie(cookie.Value) {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" {
		token = r.FormValue("csrf_token")
	}
	want := c.sign(cookie.Value)
	return subtle.ConstantTimeCompare([]byte(token), []byte(want)) == 1
}
func sameOrigin(r *http.Request, want *url.URL) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = r.Referer()
		if origin == "" {
			return false
		}
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if want == nil {
		return (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Host, r.Host)
	}
	return strings.EqualFold(u.Scheme, want.Scheme) && strings.EqualFold(u.Host, want.Host)
}
