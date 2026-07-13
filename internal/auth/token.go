package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const apiAudience = "agent-transcripts-api"

type tokenClaims struct {
	Subject   string `json:"sub"`
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	ID        string `json:"jti"`
}
type TokenManager struct {
	key []byte
	now func() time.Time
}

func NewTokenManager(key []byte) (*TokenManager, error) {
	if len(key) < 32 {
		return nil, errors.New("token key must be at least 32 bytes")
	}
	return &TokenManager{key: append([]byte(nil), key...), now: time.Now}, nil
}
func (m *TokenManager) Mint(id Identity) (string, error) {
	key, ok := normalizedIdentity(id.Key, id.DisplayName)
	if !ok {
		return "", errors.New("invalid identity")
	}
	now := m.now().UTC()
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	claims := tokenClaims{Subject: key.Key, Audience: apiAudience, IssuedAt: now.Unix(), ExpiresAt: now.Add(15 * time.Minute).Unix(), ID: base64.RawURLEncoding.EncodeToString(random)}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	input := head + "." + body
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (m *TokenManager) Verify(token string) (Identity, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, false
	}
	input := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, false
	}
	mac := hmac.New(sha256.New, m.key)
	mac.Write([]byte(input))
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return Identity{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Identity{}, false
	}
	var c tokenClaims
	if json.Unmarshal(payload, &c) != nil || c.Audience != apiAudience || c.ID == "" || c.ExpiresAt <= m.now().Unix() || c.IssuedAt > m.now().Add(time.Minute).Unix() {
		return Identity{}, false
	}
	id, ok := normalizedIdentity(c.Subject, "")
	return id, ok
}

// APIIdentity accepts a bearer token and deliberately ignores ambient cookie
// or proxy identity whenever an Authorization header is present.
func (m *TokenManager) APIIdentity(r *http.Request) (Identity, bool, bool) {
	v := r.Header.Get("Authorization")
	if v == "" {
		return Identity{}, false, false
	}
	fields := strings.Fields(v)
	if len(fields) != 2 || !strings.EqualFold(fields[0], "Bearer") {
		return Identity{}, false, true
	}
	id, ok := m.Verify(fields[1])
	return id, ok, true
}
