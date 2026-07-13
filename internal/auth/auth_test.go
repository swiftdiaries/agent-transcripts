package auth

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestProxyIdentity(t *testing.T) {
	_, network, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy("X-User", "X-Name", []*net.IPNet{network})
	h := p.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := FromContext(r.Context())
		if !ok {
			t.Fatal("missing identity")
		}
		fmt.Fprint(w, id.Key+"|"+id.DisplayName)
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-User", "Ada@Example.COM")
	req.Header.Set("X-Name", "Ada")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Body.String() != "ada@example.com|Ada" {
		t.Fatalf("body = %q", rr.Body.String())
	}
}

func TestOIDCCallbackCreatesSession(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuerURL := "https://issuer.test"
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body []byte
		switch req.URL.Path {
		case "/.well-known/openid-configuration":
			body, _ = json.Marshal(map[string]string{"issuer": issuerURL, "authorization_endpoint": issuerURL + "/authorize", "token_endpoint": issuerURL + "/token", "jwks_uri": issuerURL + "/keys"})
		case "/keys":
			body, _ = json.Marshal(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "one", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
		default:
			return nil, fmt.Errorf("unexpected URL %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})
	o, err := NewOIDC(OIDCConfig{Issuer: issuerURL, ClientID: "client", ClientSecret: "secret", RedirectURL: "https://app.example.com/auth/callback", CookieKeys: [][]byte{make([]byte, 32)}, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	login := httptest.NewRecorder()
	o.login(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	stateCookie := login.Result().Cookies()[0]
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(stateCookie)
	state, ok := o.readState(r)
	if !ok {
		t.Fatal("missing encrypted state")
	}
	claims := map[string]any{"iss": issuerURL, "aud": "client", "exp": time.Now().Add(time.Hour).Unix(), "nonce": state.Nonce, "email": "Ada@Example.COM", "name": "Ada"}
	payload, _ := json.Marshal(claims)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"one"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(header + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	token := header + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
	// The issuer test endpoint returns its request's supplied id_token via a
	// transport wrapper, avoiding credentials or callback data in logs.
	o.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/token" {
			b, _ := json.Marshal(map[string]string{"id_token": token})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
		}
		return transport(req)
	})}
	callback := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+url.QueryEscape(state.State)+"&code=opaque", nil)
	callback.AddCookie(stateCookie)
	rr := httptest.NewRecorder()
	o.callback(rr, callback)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d", rr.Code)
	}
	if len(rr.Result().Cookies()) < 2 {
		t.Fatal("session cookie missing")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProxyRejectsUntrustedPeer(t *testing.T) {
	_, network, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	p := NewProxy("X-User", "", []*net.IPNet{network})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-User", "ada@example.com")
	rr := httptest.NewRecorder()
	p.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("called") })).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestBearerIsAudienceBoundAndExpires(t *testing.T) {
	m, err := NewTokenManager(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Unix(1000, 0) }
	token, err := m.Mint(Identity{Key: "Ada@Example.COM"})
	if err != nil {
		t.Fatal(err)
	}
	if id, ok := m.Verify(token); !ok || id.Key != "ada@example.com" {
		t.Fatalf("identity = %#v, ok=%v", id, ok)
	}
	m.now = func() time.Time { return time.Unix(1000+16*60, 0) }
	if _, ok := m.Verify(token); ok {
		t.Fatal("accepted expired token")
	}
}

func TestCSRFChecksOriginOrSameOriginReferer(t *testing.T) {
	c, err := NewCSRF(bytes.Repeat([]byte("k"), 32), "https://app.example.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		origin, referer string
		want            bool
	}{{"https://app.example.com", "", true}, {"", "https://app.example.com/sessions/x", true}, {"https://evil.example", "https://app.example.com/", false}, {"", "", false}, {"not a URL", "", false}} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Referer", test.referer)
		if got := sameOrigin(r, c.origin); got != test.want {
			t.Fatalf("origin=%q referer=%q got=%v", test.origin, test.referer, got)
		}
	}
}

func TestOIDCRejectsCleartextIssuer(t *testing.T) {
	if _, err := NewOIDC(OIDCConfig{Issuer: "http://issuer.example", ClientID: "x", ClientSecret: "y", RedirectURL: "https://app.example/callback", CookieKeys: [][]byte{make([]byte, 32)}}); err == nil {
		t.Fatal("accepted cleartext issuer")
	}
}
