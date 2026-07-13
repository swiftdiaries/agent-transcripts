package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

const minKeyBytes = 32

type Config struct {
	Mode              string        `yaml:"mode"`
	Listen            string        `yaml:"listen"`
	ExternalOrigin    string        `yaml:"external_origin"`
	QuietPeriod       time.Duration `yaml:"quiet_period"`
	UploadLimits      UploadLimits  `yaml:"upload_limits"`
	Storage           Storage       `yaml:"storage"`
	Auth              Auth          `yaml:"auth"`
	TrustedProxyCIDRs []string      `yaml:"trusted_proxy_cidrs"`
	SourceRoots       []string      `yaml:"source_roots"`
	CookieKeyEnvs     []string      `yaml:"cookie_key_envs"`
	TokenKeyEnv       string        `yaml:"token_key_env"`

	CookieKeys [][]byte `yaml:"-"`
	TokenKey   []byte   `yaml:"-"`
}

type UploadLimits struct {
	SourceBytes      int64 `yaml:"source_bytes"`
	RecordBytes      int   `yaml:"record_bytes"`
	TitleBytes       int   `yaml:"title_bytes"`
	DescriptionBytes int   `yaml:"description_bytes"`
	Tags             int   `yaml:"tags"`
	TagBytes         int   `yaml:"tag_bytes"`
}

type Storage struct {
	Type     string `yaml:"type"`
	Root     string `yaml:"root"`
	Bucket   string `yaml:"bucket"`
	Prefix   string `yaml:"prefix"`
	Endpoint string `yaml:"endpoint"`
	Region   string `yaml:"region"`
}

type Auth struct {
	Type  string    `yaml:"type"`
	Proxy ProxyAuth `yaml:"proxy"`
	OIDC  OIDCAuth  `yaml:"oidc"`
}

type ProxyAuth struct {
	UserHeader string `yaml:"user_header"`
	NameHeader string `yaml:"name_header"`
}

type OIDCAuth struct {
	Issuer          string `yaml:"issuer"`
	ClientID        string `yaml:"client_id"`
	ClientSecretEnv string `yaml:"client_secret_env"`
	RedirectURL     string `yaml:"redirect_url"`
	ClientSecret    string `yaml:"-"`
}

type Overrides struct {
	Mode           *string
	Listen         *string
	ExternalOrigin *string
	StorageType    *string
	StorageRoot    *string
	AuthType       *string
}

func defaults() Config {
	return Config{
		Mode: "local", Listen: "127.0.0.1:8080", QuietPeriod: 5 * time.Minute,
		UploadLimits: UploadLimits{SourceBytes: 64 << 20, RecordBytes: 16 << 20, TitleBytes: 200, DescriptionBytes: 4 << 10, Tags: 20, TagBytes: 64},
		Storage:      Storage{Type: "filesystem", Root: "./agent-transcripts-library"},
		Auth:         Auth{Type: "local"},
	}
}

func Load(path string, overrides Overrides) (Config, error) {
	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
		if containsSecretValue(data) {
			return Config{}, errors.New("secret values must not be embedded in YAML; configure environment variable names instead")
		}
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("decode config: %w", err)
		}
	}
	loadSecrets(&cfg)
	applyOverrides(&cfg, overrides)
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func containsSecretValue(data []byte) bool {
	var node yaml.Node
	if yaml.Unmarshal(data, &node) != nil || len(node.Content) == 0 {
		return false
	}
	secretFields := map[string]bool{"cookie_key": true, "cookie_keys": true, "token_key": true, "client_secret": true}
	var walk func(*yaml.Node) bool
	walk = func(n *yaml.Node) bool {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				if secretFields[n.Content[i].Value] {
					return true
				}
				if walk(n.Content[i+1]) {
					return true
				}
			}
		}
		for _, child := range n.Content {
			if walk(child) {
				return true
			}
		}
		return false
	}
	return walk(node.Content[0])
}

func loadSecrets(cfg *Config) {
	cfg.CookieKeys = nil
	for _, name := range cfg.CookieKeyEnvs {
		if value, ok := os.LookupEnv(name); ok {
			cfg.CookieKeys = append(cfg.CookieKeys, []byte(value))
		}
	}
	if value, ok := os.LookupEnv(cfg.TokenKeyEnv); cfg.TokenKeyEnv != "" && ok {
		cfg.TokenKey = []byte(value)
	}
	if value, ok := os.LookupEnv(cfg.Auth.OIDC.ClientSecretEnv); cfg.Auth.OIDC.ClientSecretEnv != "" && ok {
		cfg.Auth.OIDC.ClientSecret = value
	}
}

func applyOverrides(cfg *Config, o Overrides) {
	if o.Mode != nil {
		cfg.Mode = *o.Mode
	}
	if o.Listen != nil {
		cfg.Listen = *o.Listen
	}
	if o.ExternalOrigin != nil {
		cfg.ExternalOrigin = *o.ExternalOrigin
	}
	if o.StorageType != nil {
		cfg.Storage.Type = *o.StorageType
	}
	if o.StorageRoot != nil {
		cfg.Storage.Root = *o.StorageRoot
	}
	if o.AuthType != nil {
		cfg.Auth.Type = *o.AuthType
	}
}

func (cfg Config) validate() error {
	if cfg.Mode != "local" && cfg.Mode != "hosted" {
		return fmt.Errorf("mode must be local or hosted")
	}
	if cfg.Mode == "local" && !isLoopbackListen(cfg.Listen) {
		return fmt.Errorf("local mode listen must use a loopback address")
	}
	if cfg.Storage.Type != "filesystem" && cfg.Storage.Type != "s3" {
		return fmt.Errorf("storage.type must be filesystem or s3")
	}
	if cfg.Storage.Type == "filesystem" && cfg.Storage.Root == "" {
		return fmt.Errorf("storage.root is required for filesystem storage")
	}
	if cfg.Storage.Type == "s3" && cfg.Storage.Bucket == "" {
		return fmt.Errorf("storage.bucket is required for s3 storage")
	}
	if cfg.Storage.Type == "s3" && cfg.Storage.Endpoint != "" {
		u, err := url.Parse(cfg.Storage.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("storage.endpoint must be an absolute URL")
		}
	}
	if err := cfg.UploadLimits.validate(); err != nil {
		return err
	}
	for _, root := range cfg.SourceRoots {
		if root == "" {
			return errors.New("source_roots must not contain empty paths")
		}
	}
	if cfg.Auth.Type != "local" && cfg.Auth.Type != "proxy" && cfg.Auth.Type != "oidc" {
		return fmt.Errorf("auth.type must be local, proxy, or oidc")
	}
	if cfg.Mode == "local" && cfg.Auth.Type != "local" {
		return fmt.Errorf("local mode requires local auth")
	}
	if cfg.Mode == "hosted" && cfg.Auth.Type == "local" {
		return fmt.Errorf("hosted mode requires proxy or oidc auth")
	}
	if cfg.Mode != "hosted" {
		return nil
	}
	u, err := url.Parse(cfg.ExternalOrigin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("external_origin must be an absolute URL")
	}
	if u.Scheme != "https" {
		return fmt.Errorf("external_origin must use https in hosted mode")
	}
	if len(cfg.CookieKeys) == 0 || len(cfg.CookieKeys[0]) < minKeyBytes {
		return fmt.Errorf("cookie_key_envs must provide a current key of at least 32 bytes")
	}
	if len(cfg.TokenKey) < minKeyBytes {
		return fmt.Errorf("token_key_env must provide a key of at least 32 bytes")
	}
	if cfg.Auth.Type == "proxy" {
		if cfg.Auth.Proxy.UserHeader == "" {
			return fmt.Errorf("auth.proxy.user_header is required")
		}
		if len(cfg.TrustedProxyCIDRs) == 0 {
			return fmt.Errorf("trusted_proxy_cidrs are required for proxy auth")
		}
		for _, cidr := range cfg.TrustedProxyCIDRs {
			if _, _, err := net.ParseCIDR(cidr); err != nil {
				return fmt.Errorf("trusted_proxy_cidrs contains invalid CIDR %q", cidr)
			}
		}
	}
	if cfg.Auth.Type == "oidc" {
		if cfg.Auth.OIDC.Issuer == "" || cfg.Auth.OIDC.ClientID == "" || cfg.Auth.OIDC.ClientSecretEnv == "" {
			return fmt.Errorf("oidc issuer, client_id, and client_secret_env are required")
		}
		if cfg.Auth.OIDC.ClientSecret == "" {
			return fmt.Errorf("auth.oidc.client_secret_env %q is missing or empty", cfg.Auth.OIDC.ClientSecretEnv)
		}
		if cfg.Auth.OIDC.RedirectURL != "" {
			redirect, err := url.Parse(cfg.Auth.OIDC.RedirectURL)
			if err != nil || redirect.Scheme != "https" || redirect.Host == "" {
				return fmt.Errorf("auth.oidc.redirect_url must use https")
			}
			if redirect.Scheme != u.Scheme || redirect.Host != u.Host {
				return fmt.Errorf("auth.oidc.redirect_url must be bound to external_origin")
			}
			if redirect.Path != "/auth/callback" || redirect.RawQuery != "" || redirect.Fragment != "" {
				return fmt.Errorf("auth.oidc.redirect_url must use /auth/callback without query or fragment")
			}
		}
	}
	return nil
}

func isLoopbackListen(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (limits UploadLimits) validate() error {
	checks := []struct {
		name  string
		value int64
		max   int64
	}{
		{"source_bytes", limits.SourceBytes, 64 << 20},
		{"record_bytes", int64(limits.RecordBytes), 16 << 20},
		{"title_bytes", int64(limits.TitleBytes), 200},
		{"description_bytes", int64(limits.DescriptionBytes), 4 << 10},
		{"tags", int64(limits.Tags), 20},
		{"tag_bytes", int64(limits.TagBytes), 64},
	}
	for _, check := range checks {
		if check.value <= 0 || check.value > check.max {
			return fmt.Errorf("upload_limits.%s must be between 1 and %d", check.name, check.max)
		}
	}
	return nil
}
