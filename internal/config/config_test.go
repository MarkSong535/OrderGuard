package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func noFiles(string) ([]byte, error) { return nil, errors.New("no files in test") }

func TestDefaults(t *testing.T) {
	c, err := load(env(map[string]string{"ORDERGUARD_API_KEYS_FILE": "keys.json"}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || c.Provider != "mock" || c.Orders.Retry.MaxAttempts != 3 ||
		c.Orders.AttemptTimeout != 2*time.Second || c.WriteTimeout() != 10*time.Second {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestShopifyTokenFromFile(t *testing.T) {
	c, err := load(env(map[string]string{
		"ORDERGUARD_API_KEYS":       "[]",
		"ORDERGUARD_PROVIDER":       "shopify",
		"SHOPIFY_SHOP_DOMAIN":       "acme.myshopify.com",
		"SHOPIFY_ACCESS_TOKEN_FILE": "/run/secrets/shopify",
	}), func(p string) ([]byte, error) {
		if p != "/run/secrets/shopify" {
			t.Fatalf("read %q", p)
		}
		return []byte("shpat_secret\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Shopify.AccessToken != "shpat_secret" {
		t.Fatalf("token = %q", c.Shopify.AccessToken)
	}
	if r := c.Redacted(); r.Shopify.AccessToken != "[redacted]" || r.APIKeysJSON != "[redacted]" {
		t.Fatal("Redacted leaked secrets")
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]map[string]string{
		"no keys":          {},
		"unknown provider": {"ORDERGUARD_API_KEYS_FILE": "k", "ORDERGUARD_PROVIDER": "magento"},
		"shopify no token": {"ORDERGUARD_API_KEYS_FILE": "k", "ORDERGUARD_PROVIDER": "shopify", "SHOPIFY_SHOP_DOMAIN": "a.myshopify.com"},
		"bad duration":     {"ORDERGUARD_API_KEYS_FILE": "k", "ORDERGUARD_UPSTREAM_ATTEMPT_TIMEOUT": "soon"},
		"attempt > budget": {"ORDERGUARD_API_KEYS_FILE": "k", "ORDERGUARD_UPSTREAM_ATTEMPT_TIMEOUT": "10s", "ORDERGUARD_UPSTREAM_REQUEST_TIMEOUT": "1s"},
		"too many retries": {"ORDERGUARD_API_KEYS_FILE": "k", "ORDERGUARD_RETRY_MAX_ATTEMPTS": "50"},
		"bad failure rate": {"ORDERGUARD_API_KEYS_FILE": "k", "ORDERGUARD_MOCK_FAILURE_RATE": "2"},
		"negative int":     {"ORDERGUARD_API_KEYS_FILE": "k", "ORDERGUARD_RATE_LIMIT_BURST": "-1"},
		"bad bool":         {"ORDERGUARD_API_KEYS_FILE": "k", "SHOPIFY_ALLOW_INSECURE_ENDPOINT": "maybe"},
	}
	for name, m := range cases {
		if _, err := load(env(m), noFiles); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	_, err := load(env(map[string]string{
		"ORDERGUARD_API_KEYS_FILE":      "k",
		"ORDERGUARD_PROVIDER":           "shopify",
		"SHOPIFY_ACCESS_TOKEN":          "shpat_supersecret",
		"ORDERGUARD_RETRY_MAX_ATTEMPTS": "0",
	}), noFiles)
	if err == nil || strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("err = %v", err)
	}
}

func TestDemoModeDefaults(t *testing.T) {
	c, err := load(env(map[string]string{"ORDERGUARD_DEMO": "true"}), noFiles)
	if err != nil {
		t.Fatalf("demo mode should not need API keys: %v", err)
	}
	if !c.Demo.Enabled || c.Orders.AttemptTimeout != 800*time.Millisecond || c.Orders.BreakerThreshold != 6 || c.RateLimitBurst != 60 {
		t.Fatalf("demo defaults not applied: %+v", c)
	}
	c, _ = load(env(map[string]string{"ORDERGUARD_DEMO": "true", "ORDERGUARD_BREAKER_FAILURE_THRESHOLD": "9"}), noFiles)
	if c.Orders.BreakerThreshold != 9 {
		t.Fatal("explicit settings must override demo defaults")
	}
	if _, err := load(env(map[string]string{"ORDERGUARD_DEMO": "true", "OPENAI_BASE_URL": "http://api.example.com/v1"}), noFiles); err == nil {
		t.Fatal("plain-http model endpoint should be rejected")
	}
	if r := (Config{Demo: DemoConfig{OpenAIKey: "sk-x"}}).Redacted(); r.Demo.OpenAIKey != "[redacted]" {
		t.Fatal("OpenAI key not redacted")
	}
	for addr, want := range map[string]string{":8080": "http://127.0.0.1:8080/mcp", "127.0.0.1:8095": "http://127.0.0.1:8095/mcp", "[::]:9000": "http://127.0.0.1:9000/mcp"} {
		if got := (Config{Addr: addr}).DemoMCPURL(); got != want {
			t.Errorf("DemoMCPURL(%q) = %q, want %q", addr, got, want)
		}
	}
}
