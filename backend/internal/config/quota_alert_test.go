package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestQuotaAlertDefaultsDisabled(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	setDefaults()
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		t.Fatalf("unmarshal defaults: %v", err)
	}
	if cfg.QuotaAlert.Enabled {
		t.Fatal("quota_alert.enabled must default to false")
	}
}

func TestQuotaAlertWebhookEndpointsLoadFromYAML(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.yaml")
	content := []byte(`quota_alert:
  enabled: true
  webhook_allowlist:
    - "http://192.0.2.10:19080/hook"
  webhook_endpoints:
    - url: "http://192.0.2.10:19080/hook"
      adapter: "generic"
`)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("DATA_DIR", configDir)

	cfg, err := LoadForBootstrap()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.QuotaAlert.Enabled {
		t.Fatal("quota alert must be enabled")
	}
	if got := len(cfg.QuotaAlert.Endpoints); got != 1 {
		t.Fatalf("endpoints = %d, want 1", got)
	}
	if got := len(cfg.QuotaAlert.Allowlist); got != 1 {
		t.Fatalf("allowlist = %d, want 1", got)
	}
	endpoint := cfg.QuotaAlert.Endpoints[0]
	if endpoint.URL != "http://192.0.2.10:19080/hook" {
		t.Fatalf("endpoint URL = %q", endpoint.URL)
	}
	if endpoint.Adapter != "generic" {
		t.Fatalf("endpoint adapter = %q", endpoint.Adapter)
	}
}
