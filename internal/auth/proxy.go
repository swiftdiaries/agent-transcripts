package auth

import (
	"net"
	"net/http"
	"strings"
)

// Proxy trusts identity headers only when the TCP peer is in trustedCIDRs.
// It deliberately does not consult Forwarded/X-Forwarded-For, which a client
// can forge unless a proxy has separately stripped it.
type Proxy struct {
	userHeader, nameHeader string
	trusted                []*net.IPNet
}

func NewProxy(userHeader, nameHeader string, trustedCIDRs []*net.IPNet) *Proxy {
	return &Proxy{userHeader: http.CanonicalHeaderKey(userHeader), nameHeader: http.CanonicalHeaderKey(nameHeader), trusted: trustedCIDRs}
}

func ParseCIDRs(values []string) ([]*net.IPNet, error) {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, n, err := net.ParseCIDR(value)
		if err != nil {
			return nil, err
		}
		result = append(result, n)
	}
	return result, nil
}

func (p *Proxy) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bearer requests are exclusively handled by API token middleware.
		if strings.TrimSpace(r.Header.Get("Authorization")) != "" {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				next.ServeHTTP(w, r)
				return
			}
			unauthorized(w)
			return
		}
		peer, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(peer)
		trusted := err == nil && ip != nil
		if trusted {
			trusted = false
			for _, cidr := range p.trusted {
				if cidr.Contains(ip) {
					trusted = true
					break
				}
			}
		}
		if !trusted {
			unauthorized(w)
			return
		}
		id, ok := normalizedIdentity(r.Header.Get(p.userHeader), r.Header.Get(p.nameHeader))
		if !ok {
			unauthorized(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}
