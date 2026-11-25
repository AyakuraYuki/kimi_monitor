package config

import (
	"strings"
	"testing"
)

func TestWebConfigValidation(t *testing.T) {
	valid := WebConfig{
		Listen: "127.0.0.1:8080",
		OIDC:   OIDCConfig{Issuer: "https://identity.example/application/o/monitor/", ClientID: "monitor", ClientSecret: "secret"},
	}
	for _, tc := range []struct {
		name   string
		change func(*WebConfig)
		ok     bool
	}{
		{"oidc", func(w *WebConfig) {}, true},
		{"no login", func(w *WebConfig) { w.OIDC = OIDCConfig{} }, false},
		{"invalid local hash", func(w *WebConfig) { w.Username = "admin"; w.PasswordHash = "plain password" }, false},
		{"damaged bcrypt encoding", func(w *WebConfig) { w.Username = "admin"; w.PasswordHash = "$2a$10$" + strings.Repeat("!", 53) }, false},
		{"missing secret", func(w *WebConfig) { w.OIDC.ClientSecret = "" }, false},
		{"non-TLS issuer", func(w *WebConfig) { w.OIDC.Issuer = "http://identity.example/" }, false},
		{"wildcard", func(w *WebConfig) { w.Listen = ":8080" }, true},
		{"IPv4", func(w *WebConfig) { w.Listen = "192.0.2.1:443" }, true},
		{"IPv6", func(w *WebConfig) { w.Listen = "[2001:db8::1]:8443" }, true},
		{"invalid port", func(w *WebConfig) { w.Listen = ":65536" }, false},
		{"missing port", func(w *WebConfig) { w.Listen = "[::1]" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := valid
			tc.change(&w)
			if err := w.Validate(); (err == nil) != tc.ok {
				t.Fatalf("Validate(): %v", err)
			}
		})
	}
}

func TestLoadWebOIDCWithDefaultListen(t *testing.T) {
	const body = `
database:
  dsn: postgres://localhost/test
web:
  enabled: true
  oidc:
    issuer: https://auth.example/application/o/kimi/
    client_id: monitor
    client_secret: local-test
`
	cfg, err := Load(writeConfig(t, t.TempDir(), body, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Web.Enabled || cfg.Web.Listen != "127.0.0.1:8080" || cfg.Web.OIDC.ClientID != "monitor" {
		t.Fatal("未能加载 OIDC 配置或默认监听地址")
	}
}
