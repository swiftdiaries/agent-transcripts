package auth

// This OIDC implementation intentionally uses only the standard library so
// the server's trust decisions remain small and auditable.
import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type OIDCConfig struct {
	Issuer, ClientID, ClientSecret, RedirectURL string
	CookieKeys                                  [][]byte
	HTTPClient                                  *http.Client
	SessionLifetime                             time.Duration
	AllowInsecureTest                           bool
}
type OIDC struct {
	cfg    OIDCConfig
	client *http.Client
	now    func() time.Time
}
type oidcDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	Issuer                string `json:"issuer"`
}
type oidcState struct {
	State, Nonce, Verifier string
	Expires                int64
}
type oidcSession struct {
	Key, Name string
	Expires   int64
}

func NewOIDC(cfg OIDCConfig) (*OIDC, error) {
	u, err := url.Parse(cfg.Issuer)
	if err != nil || (u.Scheme != "https" && !cfg.AllowInsecureTest) || u.Host == "" || cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" || len(cfg.CookieKeys) == 0 || len(cfg.CookieKeys[0]) < 32 {
		return nil, errors.New("invalid OIDC configuration")
	}
	for _, k := range cfg.CookieKeys {
		if len(k) < 32 {
			return nil, errors.New("invalid OIDC cookie key")
		}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.SessionLifetime <= 0 {
		cfg.SessionLifetime = 8 * time.Hour
	}
	return &OIDC{cfg: cfg, client: cfg.HTTPClient, now: time.Now}, nil
}
func (o *OIDC) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/login" {
			o.login(w, r)
			return
		}
		if r.URL.Path == "/auth/callback" {
			o.callback(w, r)
			return
		}
		if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				next.ServeHTTP(w, r)
				return
			}
			unauthorized(w)
			return
		}
		if s, ok := o.readSession(r); ok {
			id, valid := normalizedIdentity(s.Key, s.Name)
			if valid {
				next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
				return
			}
		}
		unauthorized(w)
	})
}
func (o *OIDC) discovery() (oidcDiscovery, error) {
	var d oidcDiscovery
	u := strings.TrimSuffix(o.cfg.Issuer, "/") + "/.well-known/openid-configuration"
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	resp, err := o.client.Do(req)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&d) != nil || d.Issuer != o.cfg.Issuer || !o.secureEndpoint(d.AuthorizationEndpoint) || !o.secureEndpoint(d.TokenEndpoint) || !o.secureEndpoint(d.JWKSURI) {
		return d, errors.New("invalid OIDC discovery")
	}
	return d, nil
}

func (o *OIDC) secureEndpoint(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Host != "" && (u.Scheme == "https" || o.cfg.AllowInsecureTest)
}
func randomURL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func (o *OIDC) login(w http.ResponseWriter, r *http.Request) {
	d, err := o.discovery()
	if err != nil {
		unauthorized(w)
		return
	}
	state, nonce, verifier := randomURL(32), randomURL(32), randomURL(48)
	o.writeCookie(w, "agent_transcripts_oidc_state", oidcState{state, nonce, verifier, o.now().Add(10 * time.Minute).Unix()}, 10*time.Minute)
	challenge := sha256.Sum256([]byte(verifier))
	u, err := url.Parse(d.AuthorizationEndpoint)
	if err != nil {
		unauthorized(w)
		return
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", o.cfg.ClientID)
	q.Set("redirect_uri", o.cfg.RedirectURL)
	q.Set("scope", "openid email profile")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}
func (o *OIDC) callback(w http.ResponseWriter, r *http.Request) {
	st, ok := o.readState(r)
	if !ok || st.Expires < o.now().Unix() || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(st.State)) != 1 || r.URL.Query().Get("code") == "" {
		unauthorized(w)
		return
	}
	d, err := o.discovery()
	if err != nil {
		unauthorized(w)
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {r.URL.Query().Get("code")}, "redirect_uri": {o.cfg.RedirectURL}, "client_id": {o.cfg.ClientID}, "client_secret": {o.cfg.ClientSecret}, "code_verifier": {st.Verifier}}
	resp, err := o.client.PostForm(d.TokenEndpoint, form)
	if err != nil {
		unauthorized(w)
		return
	}
	defer resp.Body.Close()
	var tr struct {
		IDToken string `json:"id_token"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&tr) != nil {
		unauthorized(w)
		return
	}
	id, ok := o.verifyIDToken(d, tr.IDToken, st.Nonce)
	if !ok {
		unauthorized(w)
		return
	} // rotate away transient state and issue a new encrypted session.
	o.writeCookie(w, "agent_transcripts_oidc_state", oidcState{}, -1)
	o.writeCookie(w, "agent_transcripts_oidc_session", oidcSession{id.Key, id.DisplayName, o.now().Add(o.cfg.SessionLifetime).Unix()}, o.cfg.SessionLifetime)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
func (o *OIDC) verifyIDToken(d oidcDiscovery, token, nonce string) (Identity, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Identity{}, false
	}
	var h struct{ Alg, Kid string }
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(raw, &h) != nil || h.Alg != "RS256" || h.Kid == "" {
		return Identity{}, false
	}
	var claims struct {
		Iss                     string      `json:"iss"`
		Aud                     interface{} `json:"aud"`
		Exp                     int64       `json:"exp"`
		Nonce, Email, Name, Sub string
	}
	raw, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &claims) != nil || claims.Iss != o.cfg.Issuer || claims.Exp <= o.now().Unix() || claims.Nonce != nonce || !hasAudience(claims.Aud, o.cfg.ClientID) {
		return Identity{}, false
	}
	key, ok := o.jwk(d.JWKSURI, h.Kid)
	if !ok {
		return Identity{}, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Identity{}, false
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return Identity{}, false
	}
	subject := claims.Email
	if subject == "" {
		subject = claims.Sub
	}
	return normalizedIdentity(subject, claims.Name)
}
func hasAudience(v interface{}, id string) bool {
	switch x := v.(type) {
	case string:
		return x == id
	case []interface{}:
		for _, e := range x {
			if s, _ := e.(string); s == id {
				return true
			}
		}
	}
	return false
}
func (o *OIDC) jwk(uri, kid string) (*rsa.PublicKey, bool) {
	resp, err := o.client.Get(uri)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	var doc struct {
		Keys []struct{ Kty, Kid, N, E string } `json:"keys"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&doc) != nil {
		return nil, false
	}
	for _, k := range doc.Keys {
		if k.Kty == "RSA" && k.Kid == kid {
			n, e1 := base64.RawURLEncoding.DecodeString(k.N)
			e, e2 := base64.RawURLEncoding.DecodeString(k.E)
			if e1 != nil || e2 != nil {
				return nil, false
			}
			ei := 0
			for _, b := range e {
				ei = ei<<8 | int(b)
			}
			if ei < 3 {
				return nil, false
			}
			return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: ei}, true
		}
	}
	return nil, false
}
func (o *OIDC) writeCookie(w http.ResponseWriter, name string, v interface{}, life time.Duration) {
	value, err := o.seal(v)
	if err != nil {
		return
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: int(life.Seconds())})
}
func (o *OIDC) readState(r *http.Request) (oidcState, bool) {
	var s oidcState
	ok := o.openCookie(r, "agent_transcripts_oidc_state", &s)
	return s, ok
}
func (o *OIDC) readSession(r *http.Request) (oidcSession, bool) {
	var s oidcSession
	ok := o.openCookie(r, "agent_transcripts_oidc_session", &s)
	return s, ok && s.Expires > o.now().Unix()
}
func (o *OIDC) openCookie(r *http.Request, name string, v interface{}) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	for _, k := range o.cfg.CookieKeys {
		if raw, ok := open(k, c.Value); ok && json.Unmarshal(raw, v) == nil {
			return true
		}
	}
	return false
}
func (o *OIDC) seal(v interface{}) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return seal(o.cfg.CookieKeys[0], raw)
}
func seal(key, plain []byte) (string, error) {
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := g.Seal(nil, nonce, plain, nil)
	mac := hmac.New(sha256.New, key)
	mac.Write(nonce)
	mac.Write(ciphertext)
	return base64.RawURLEncoding.EncodeToString(append(append(nonce, ciphertext...), mac.Sum(nil)...)), nil
}
func open(key []byte, value string) ([]byte, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, false
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, false
	}
	g, err := cipher.NewGCM(block)
	if err != nil || len(raw) < g.NonceSize()+sha256.Size {
		return nil, false
	}
	nonce := raw[:g.NonceSize()]
	macStart := len(raw) - sha256.Size
	mac := hmac.New(sha256.New, key)
	mac.Write(raw[:macStart])
	if subtle.ConstantTimeCompare(raw[macStart:], mac.Sum(nil)) != 1 {
		return nil, false
	}
	plain, err := g.Open(nil, nonce, raw[g.NonceSize():macStart], nil)
	return plain, err == nil
}
