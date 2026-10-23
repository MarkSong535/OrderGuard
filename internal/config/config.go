// Package config loads service configuration from environment variables.
// Secrets are read from the environment or from files (for Docker/Kubernetes
// secrets) and are never given defaults.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"orderguard/internal/orders"
	"orderguard/internal/resilience"
)

// Config is the complete service configuration.
type Config struct {
	Addr            string
	LogLevel        string
	LogFormat       string
	ShutdownTimeout time.Duration

	APIKeysFile string // path to JSON key table
	APIKeysJSON string // inline JSON key table (alternative to the file)

	Provider string // "mock" or "shopify"
	Shopify  ShopifyConfig
	Mock     MockConfig

	Orders orders.Config

	MaxInFlight    int64
	RateLimitRPS   float64
	RateLimitBurst int

	Demo DemoConfig
}

// DemoConfig configures the interactive demo (see internal/demo).
type DemoConfig struct {
	Enabled         bool
	OpenAIKey       string // enables the AI assistant
	OpenAIBaseURL   string
	AssistantModel  string
	ReasoningEffort string
	MCPURL          string // "" derives http://127.0.0.1:<port>/mcp from Addr
	TrustCFHeader   bool
	ChatPerMinute   float64
	ChatDailyCap    int
	ScenarioTTL     time.Duration
	HealthyLatency  time.Duration
}

// ShopifyConfig configures the Shopify provider.
type ShopifyConfig struct {
	ShopDomain            string
	APIVersion            string
	AccessToken           string
	Endpoint              string
	AllowInsecureEndpoint bool
}

// MockConfig configures the in-memory provider's fault injection.
type MockConfig struct {
	Latency     time.Duration
	FailureRate float64
}

// Redacted returns a copy safe to log.
func (c Config) Redacted() Config {
	if c.Shopify.AccessToken != "" {
		c.Shopify.AccessToken = "[redacted]"
	}
	if c.APIKeysJSON != "" {
		c.APIKeysJSON = "[redacted]"
	}
	if c.Demo.OpenAIKey != "" {
		c.Demo.OpenAIKey = "[redacted]"
	}
	return c
}

// WriteTimeout is the HTTP server write deadline: long enough for a full
// upstream retry budget plus response encoding.
func (c Config) WriteTimeout() time.Duration { return c.Orders.RequestTimeout + 5*time.Second }

type loader struct {
	getenv func(string) string
	errs   []error
}

func (l *loader) str(key, def string) string {
	if v := strings.TrimSpace(l.getenv(key)); v != "" {
		return v
	}
	return def
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	v := l.getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid duration %q", key, v))
		return def
	}
	return d
}

func (l *loader) integer(key string, def int) int {
	v := l.getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid non-negative integer %q", key, v))
		return def
	}
	return n
}

func (l *loader) float(key string, def float64) float64 {
	v := l.getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid non-negative number %q", key, v))
		return def
	}
	return f
}

func (l *loader) boolean(key string, def bool) bool {
	v := l.getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s: invalid boolean %q", key, v))
		return def
	}
	return b
}

// secret reads KEY, or the contents of the file named by KEY_FILE.
func (l *loader) secret(key string, readFile func(string) ([]byte, error)) string {
	if v := l.getenv(key); v != "" {
		return strings.TrimSpace(v)
	}
	path := l.getenv(key + "_FILE")
	if path == "" {
		return ""
	}
	b, err := readFile(path)
	if err != nil {
		l.errs = append(l.errs, fmt.Errorf("%s_FILE: %w", key, err))
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Load reads configuration using getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	return load(getenv, os.ReadFile)
}

func load(getenv func(string) string, readFile func(string) ([]byte, error)) (Config, error) {
	l := &loader{getenv: getenv}
	demo := l.boolean("ORDERGUARD_DEMO", false)
	def := orders.DefaultConfig()
	rps, burst := 10.0, 20
	if demo {
		// Short budgets so failure scenarios are quick to watch, a breaker
		// threshold that the guided test plan's slow+flaky steps stay under,
		// and room for the page's 30-request burst on shared demo keys.
		def.AttemptTimeout = 800 * time.Millisecond
		def.RequestTimeout = 2500 * time.Millisecond
		def.Retry.BaseDelay = 150 * time.Millisecond
		def.BreakerThreshold = 6
		def.BreakerCooldown = 8 * time.Second
		rps, burst = 20, 60
	}

	c := Config{
		Addr:            l.str("ORDERGUARD_ADDR", ":8080"),
		LogLevel:        l.str("ORDERGUARD_LOG_LEVEL", "info"),
		LogFormat:       l.str("ORDERGUARD_LOG_FORMAT", "json"),
		ShutdownTimeout: l.duration("ORDERGUARD_SHUTDOWN_TIMEOUT", 15*time.Second),
		APIKeysFile:     l.str("ORDERGUARD_API_KEYS_FILE", ""),
		APIKeysJSON:     l.getenv("ORDERGUARD_API_KEYS"),
		Provider:        l.str("ORDERGUARD_PROVIDER", "mock"),
		Shopify: ShopifyConfig{
			ShopDomain:            l.str("SHOPIFY_SHOP_DOMAIN", ""),
			APIVersion:            l.str("SHOPIFY_API_VERSION", "2025-07"),
			AccessToken:           l.secret("SHOPIFY_ACCESS_TOKEN", readFile),
			Endpoint:              l.str("SHOPIFY_ENDPOINT", ""),
			AllowInsecureEndpoint: l.boolean("SHOPIFY_ALLOW_INSECURE_ENDPOINT", false),
		},
		Mock: MockConfig{
			Latency:     l.duration("ORDERGUARD_MOCK_LATENCY", 0),
			FailureRate: l.float("ORDERGUARD_MOCK_FAILURE_RATE", 0),
		},
		Orders: orders.Config{
			AttemptTimeout: l.duration("ORDERGUARD_UPSTREAM_ATTEMPT_TIMEOUT", def.AttemptTimeout),
			RequestTimeout: l.duration("ORDERGUARD_UPSTREAM_REQUEST_TIMEOUT", def.RequestTimeout),
			Retry: resilience.RetryPolicy{
				MaxAttempts: l.integer("ORDERGUARD_RETRY_MAX_ATTEMPTS", def.Retry.MaxAttempts),
				BaseDelay:   l.duration("ORDERGUARD_RETRY_BASE_DELAY", def.Retry.BaseDelay),
				MaxDelay:    l.duration("ORDERGUARD_RETRY_MAX_DELAY", def.Retry.MaxDelay),
				Jitter:      true,
			},
			BreakerThreshold: l.integer("ORDERGUARD_BREAKER_FAILURE_THRESHOLD", def.BreakerThreshold),
			BreakerCooldown:  l.duration("ORDERGUARD_BREAKER_OPEN_TIMEOUT", def.BreakerCooldown),
			MaxConcurrent:    l.integer("ORDERGUARD_UPSTREAM_MAX_CONCURRENCY", def.MaxConcurrent),
			BulkheadWait:     l.duration("ORDERGUARD_UPSTREAM_QUEUE_TIMEOUT", def.BulkheadWait),
			StaleTTL:         l.duration("ORDERGUARD_STALE_CACHE_TTL", def.StaleTTL),
			StaleCacheSize:   l.integer("ORDERGUARD_STALE_CACHE_SIZE", def.StaleCacheSize),
		},
		MaxInFlight:    int64(l.integer("ORDERGUARD_MAX_INFLIGHT", 512)),
		RateLimitRPS:   l.float("ORDERGUARD_RATE_LIMIT_RPS", rps),
		RateLimitBurst: l.integer("ORDERGUARD_RATE_LIMIT_BURST", burst),
		Demo: DemoConfig{
			Enabled:         demo,
			OpenAIKey:       l.secret("OPENAI_API_KEY", readFile),
			OpenAIBaseURL:   l.str("OPENAI_BASE_URL", "https://api.openai.com/v1"),
			AssistantModel:  l.str("ORDERGUARD_ASSISTANT_MODEL", "gpt-6-luna"),
			ReasoningEffort: l.str("ORDERGUARD_ASSISTANT_REASONING_EFFORT", "none"),
			MCPURL:          l.str("ORDERGUARD_DEMO_MCP_URL", ""),
			TrustCFHeader:   l.boolean("ORDERGUARD_DEMO_TRUST_CF_CONNECTING_IP", false),
			ChatPerMinute:   l.float("ORDERGUARD_DEMO_CHAT_PER_MINUTE", 6),
			ChatDailyCap:    l.integer("ORDERGUARD_DEMO_CHAT_DAILY_CAP", 300),
			ScenarioTTL:     l.duration("ORDERGUARD_DEMO_SCENARIO_TTL", 90*time.Second),
			HealthyLatency:  l.duration("ORDERGUARD_DEMO_UPSTREAM_LATENCY", 120*time.Millisecond),
		},
	}

	if err := errors.Join(l.errs...); err != nil {
		return Config{}, err
	}
	return c, c.validate()
}

func (c Config) validate() error {
	var errs []error
	if c.Demo.Enabled {
		// Demo mode generates its own users and runs an in-process fake Shopify.
		if u, err := url.Parse(c.Demo.OpenAIBaseURL); err != nil || u.Host == "" ||
			(u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
			errs = append(errs, errors.New("OPENAI_BASE_URL must be an https URL"))
		}
		return errors.Join(append(errs, c.validateResilience()...)...)
	}
	if c.APIKeysFile == "" && c.APIKeysJSON == "" {
		errs = append(errs, errors.New("one of ORDERGUARD_API_KEYS_FILE or ORDERGUARD_API_KEYS is required"))
	}
	switch c.Provider {
	case "mock":
		if c.Mock.FailureRate > 1 {
			errs = append(errs, errors.New("ORDERGUARD_MOCK_FAILURE_RATE must be between 0 and 1"))
		}
	case "shopify":
		if c.Shopify.AccessToken == "" {
			errs = append(errs, errors.New("SHOPIFY_ACCESS_TOKEN or SHOPIFY_ACCESS_TOKEN_FILE is required for the shopify provider"))
		}
		if c.Shopify.ShopDomain == "" && c.Shopify.Endpoint == "" {
			errs = append(errs, errors.New("SHOPIFY_SHOP_DOMAIN is required for the shopify provider"))
		}
	default:
		errs = append(errs, fmt.Errorf("ORDERGUARD_PROVIDER: unknown provider %q (want mock or shopify)", c.Provider))
	}
	return errors.Join(append(errs, c.validateResilience()...)...)
}

func (c Config) validateResilience() []error {
	var errs []error
	if c.Orders.AttemptTimeout <= 0 || c.Orders.RequestTimeout <= 0 {
		errs = append(errs, errors.New("upstream timeouts must be positive"))
	}
	if c.Orders.AttemptTimeout > c.Orders.RequestTimeout {
		errs = append(errs, errors.New("ORDERGUARD_UPSTREAM_ATTEMPT_TIMEOUT must not exceed ORDERGUARD_UPSTREAM_REQUEST_TIMEOUT"))
	}
	if c.Orders.Retry.MaxAttempts < 1 || c.Orders.Retry.MaxAttempts > 10 {
		errs = append(errs, errors.New("ORDERGUARD_RETRY_MAX_ATTEMPTS must be between 1 and 10"))
	}
	if c.Orders.MaxConcurrent < 1 {
		errs = append(errs, errors.New("ORDERGUARD_UPSTREAM_MAX_CONCURRENCY must be at least 1"))
	}
	if c.Orders.BreakerThreshold < 1 {
		errs = append(errs, errors.New("ORDERGUARD_BREAKER_FAILURE_THRESHOLD must be at least 1"))
	}
	return errs
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// DemoMCPURL is where the demo assistant reaches this server's MCP endpoint.
func (c Config) DemoMCPURL() string {
	if c.Demo.MCPURL != "" {
		return c.Demo.MCPURL
	}
	host, port, err := net.SplitHostPort(c.Addr)
	if err != nil {
		return "http://127.0.0.1:8080/mcp"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/mcp"
}
