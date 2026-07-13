// Package auth supplies the identity boundary for the HTTP application.
package auth

import (
	"context"
	"net/http"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

// Identity is the normalized, stable identity used for ownership comparisons.
type Identity struct{ Key, DisplayName string }

type Provider interface {
	Wrap(http.Handler) http.Handler
}

type identityContextKey struct{}

func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityContextKey{}).(Identity)
	return id, ok
}

func withIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, id)
}

// WithIdentity attaches an already-verified identity. It is intended only for
// authentication middleware such as TokenManager.
func WithIdentity(ctx context.Context, id Identity) context.Context { return withIdentity(ctx, id) }

func normalizedIdentity(key, name string) (Identity, bool) {
	key, err := session.NormalizeUploaderKey(key)
	if err != nil {
		return Identity{}, false
	}
	return Identity{Key: key, DisplayName: name}, true
}

type localProvider struct{}

// NewLocal supplies the deterministic local-only principal. Configuration
// prevents this provider from being used in hosted mode.
func NewLocal() Provider { return localProvider{} }
func (localProvider) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), Identity{Key: "local", DisplayName: "Local"})))
	})
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "authentication required", http.StatusUnauthorized)
}
