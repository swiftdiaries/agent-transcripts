package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesDefaultsYAMLThenOverrides(t *testing.T) {
	path := writeTempConfig(t, "mode: hosted\nlisten: \":9000\"\nstorage:\n  type: filesystem\n  root: /yaml/library\nauth:\n  type: proxy\n  proxy:\n    user_header: X-User\n")
	got, err := Load(path, Overrides{Listen: ptr(":9100")})
	if err != nil {
		t.Fatal(err)
	}
	if got.Listen != ":9100" {
		t.Fatalf("listen = %q", got.Listen)
	}
	if got.Storage.Root != "/yaml/library" {
		t.Fatalf("root = %q", got.Storage.Root)
	}
	if got.QuietPeriod != 5*time.Minute {
		t.Fatalf("quiet = %s", got.QuietPeriod)
	}
}

func TestHostedRejectsLocalIdentity(t *testing.T) {
	_, err := Load(writeTempConfig(t, "mode: hosted\nauth:\n  type: local\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "hosted mode requires proxy or oidc auth") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostedRequiresExternalOriginAndSessionKey(t *testing.T) {
	_, err := Load(writeTempConfig(t, "mode: hosted\nauth:\n  type: proxy\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "external_origin") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsUnknownYAMLFields(t *testing.T) {
	_, err := Load(writeTempConfig(t, "mode: local\nsurprise: true\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "field surprise not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsEmbeddedSecrets(t *testing.T) {
	_, err := Load(writeTempConfig(t, "mode: local\ncookie_key: top-secret\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "secret values must not be embedded in YAML") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostedValidationLoadsKeysFromEnvironment(t *testing.T) {
	t.Setenv("COOKIE_KEY", strings.Repeat("c", 32))
	t.Setenv("TOKEN_KEY", strings.Repeat("t", 32))
	path := writeTempConfig(t, "mode: hosted\nexternal_origin: https://transcripts.example.com\nstorage:\n  type: filesystem\n  root: /library\nauth:\n  type: proxy\n  proxy:\n    user_header: X-User\ntrusted_proxy_cidrs: [10.0.0.0/8]\ncookie_key_envs: [COOKIE_KEY]\ntoken_key_env: TOKEN_KEY\n")
	got, err := load(path, Overrides{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.CookieKeys) != 1 || string(got.CookieKeys[0]) != strings.Repeat("c", 32) {
		t.Fatalf("cookie keys not loaded")
	}
	if string(got.TokenKey) != strings.Repeat("t", 32) {
		t.Fatalf("token key not loaded")
	}
}

func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func ptr[T any](value T) *T { return &value }
