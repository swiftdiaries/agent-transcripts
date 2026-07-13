package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesDefaultsYAMLThenOverrides(t *testing.T) {
	setHostedKeys(t)
	path := writeTempConfig(t, "mode: hosted\nlisten: \":9000\"\nexternal_origin: https://transcripts.example.com\nstorage:\n  type: filesystem\n  root: /yaml/library\nauth:\n  type: proxy\n  proxy:\n    user_header: X-User\ntrusted_proxy_cidrs: [10.0.0.0/8]\ncookie_key_envs: [COOKIE_KEY]\ntoken_key_env: TOKEN_KEY\n")
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

func TestLocalDefaultsToLoopbackAndRejectsPublicListen(t *testing.T) {
	got, err := Load("", Overrides{})
	if err != nil || got.Listen != "127.0.0.1:8080" {
		t.Fatalf("listen = %q, err = %v", got.Listen, err)
	}
	for _, value := range []string{":8080", "0.0.0.0:8080", "192.0.2.1:8080"} {
		_, err := Load(writeTempConfig(t, "mode: local\nlisten: "+value+"\n"), Overrides{})
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("listen %q error = %v", value, err)
		}
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
	setHostedKeys(t)
	path := writeTempConfig(t, "mode: hosted\nexternal_origin: https://transcripts.example.com\nstorage:\n  type: filesystem\n  root: /library\nauth:\n  type: proxy\n  proxy:\n    user_header: X-User\ntrusted_proxy_cidrs: [10.0.0.0/8]\ncookie_key_envs: [COOKIE_KEY]\ntoken_key_env: TOKEN_KEY\n")
	got, err := Load(path, Overrides{})
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

func TestPublicLoadRequiresHTTPSInHostedMode(t *testing.T) {
	setHostedKeys(t)
	path := writeTempConfig(t, "mode: hosted\nexternal_origin: http://transcripts.example.com\nauth:\n  type: proxy\n  proxy:\n    user_header: X-User\ntrusted_proxy_cidrs: [10.0.0.0/8]\ncookie_key_envs: [COOKIE_KEY]\ntoken_key_env: TOKEN_KEY\n")
	_, err := Load(path, Overrides{})
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsInvalidUploadLimits(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"source zero", "source_bytes: 0", "upload_limits.source_bytes"},
		{"source over cap", "source_bytes: 67108865", "upload_limits.source_bytes"},
		{"record over cap", "record_bytes: 16777217", "upload_limits.record_bytes"},
		{"title over cap", "title_bytes: 201", "upload_limits.title_bytes"},
		{"description over cap", "description_bytes: 4097", "upload_limits.description_bytes"},
		{"tags over cap", "tags: 21", "upload_limits.tags"},
		{"tag zero", "tag_bytes: 0", "upload_limits.tag_bytes"},
		{"tag over cap", "tag_bytes: 65", "upload_limits.tag_bytes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writeTempConfig(t, "mode: local\nupload_limits:\n  "+test.yaml+"\n"), Overrides{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestLoadRejectsEmptySourceRoot(t *testing.T) {
	_, err := Load(writeTempConfig(t, "mode: local\nsource_roots: [\"\"]\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "source_roots") {
		t.Fatalf("error = %v", err)
	}
}

func TestS3StorageRejectsInvalidEndpoint(t *testing.T) {
	_, err := Load(writeTempConfig(t, "mode: local\nstorage:\n  type: s3\n  bucket: transcripts\n  endpoint: not-a-url\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "storage.endpoint") {
		t.Fatalf("error = %v", err)
	}
}

func TestS3StorageRejectsUnsupportedEndpointScheme(t *testing.T) {
	_, err := Load(writeTempConfig(t, "mode: local\nstorage:\n  type: s3\n  bucket: transcripts\n  endpoint: ftp://storage.example.com\n"), Overrides{})
	if err == nil || !strings.Contains(err.Error(), "storage.endpoint") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostedOIDCRequiresClientSecretEnvironmentValue(t *testing.T) {
	for _, test := range []struct {
		name     string
		setEmpty bool
	}{{name: "missing"}, {name: "empty", setEmpty: true}} {
		t.Run(test.name, func(t *testing.T) {
			setHostedKeys(t)
			if test.setEmpty {
				t.Setenv("OIDC_CLIENT_SECRET", "")
			}
			path := writeTempConfig(t, hostedOIDCYAML())
			_, err := Load(path, Overrides{})
			if err == nil || !strings.Contains(err.Error(), "client_secret_env") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestHostedOIDCLoadsClientSecretFromEnvironment(t *testing.T) {
	setHostedKeys(t)
	t.Setenv("OIDC_CLIENT_SECRET", "not-in-yaml")
	got, err := Load(writeTempConfig(t, hostedOIDCYAML()), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Auth.OIDC.ClientSecret != "not-in-yaml" {
		t.Fatalf("client secret was not loaded")
	}
}

func setHostedKeys(t *testing.T) {
	t.Helper()
	t.Setenv("COOKIE_KEY", strings.Repeat("c", 32))
	t.Setenv("TOKEN_KEY", strings.Repeat("t", 32))
}

func hostedOIDCYAML() string {
	return "mode: hosted\nexternal_origin: https://transcripts.example.com\nauth:\n  type: oidc\n  oidc:\n    issuer: https://identity.example.com\n    client_id: transcripts\n    client_secret_env: OIDC_CLIENT_SECRET\ncookie_key_envs: [COOKIE_KEY]\ntoken_key_env: TOKEN_KEY\n"
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
