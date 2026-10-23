// Command server runs the OrderGuard HTTP API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"orderguard/internal/api"
	"orderguard/internal/assistant"
	"orderguard/internal/auth"
	"orderguard/internal/config"
	"orderguard/internal/demo"
	"orderguard/internal/observability"
	"orderguard/internal/orders"
	"orderguard/internal/provider"
	"orderguard/internal/provider/fixtures"
	"orderguard/internal/provider/mock"
	"orderguard/internal/provider/shopify"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "probe the local /health endpoint and exit (for container HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		os.Exit(probe(os.Getenv("ORDERGUARD_ADDR")))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "orderguard:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, logOut io.Writer) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	log, err := observability.NewLogger(logOut, cfg.LogLevel, cfg.LogFormat)
	if err != nil {
		return err
	}
	metrics := observability.NewMetrics()

	var (
		authn *auth.Authenticator
		prov  provider.Provider
		dm    *demo.Demo
	)
	if cfg.Demo.Enabled {
		var model assistant.Model
		if cfg.Demo.OpenAIKey != "" {
			model = &assistant.OpenAI{
				BaseURL:         cfg.Demo.OpenAIBaseURL,
				APIKey:          cfg.Demo.OpenAIKey,
				Model:           cfg.Demo.AssistantModel,
				ReasoningEffort: cfg.Demo.ReasoningEffort,
				HTTP:            &http.Client{Timeout: 45 * time.Second},
			}
		}
		dm, err = demo.New(demo.Config{
			AssistantModel: model,
			MCPURL:         cfg.DemoMCPURL(),
			TrustCFHeader:  cfg.Demo.TrustCFHeader,
			ChatPerMinute:  cfg.Demo.ChatPerMinute,
			ChatDailyCap:   cfg.Demo.ChatDailyCap,
			ScenarioTTL:    cfg.Demo.ScenarioTTL,
			HealthyLatency: cfg.Demo.HealthyLatency,
		}, log)
		if err != nil {
			return fmt.Errorf("demo: %w", err)
		}
		defer dm.Close(context.Background())
		if authn, err = auth.NewAuthenticator(dm.KeyEntries()); err != nil {
			return err
		}
		if prov, err = dm.Provider(); err != nil {
			return err
		}
		log.Warn("DEMO MODE: serving synthetic fixtures from an in-process fake Shopify with generated API keys",
			"assistant", cfg.Demo.OpenAIKey != "", "assistant_model", cfg.Demo.AssistantModel)
	} else {
		if authn, err = loadAuthenticator(cfg); err != nil {
			return err
		}
		if prov, err = newProvider(cfg); err != nil {
			return err
		}
	}

	svc := orders.NewService(prov, cfg.Orders, log, metrics)
	apiCfg := api.Config{
		MaxInFlight:    cfg.MaxInFlight,
		RateLimitRPS:   cfg.RateLimitRPS,
		RateLimitBurst: cfg.RateLimitBurst,
	}
	if dm != nil {
		dm.Attach(svc)
		apiCfg.Mount = dm.Mount
	}
	handler := api.NewHandler(apiCfg, svc, authn, log, metrics)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      cfg.WriteTimeout(),
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("starting server", "addr", cfg.Addr, "provider", prov.Name(),
			"attempt_timeout", cfg.Orders.AttemptTimeout.String(),
			"request_timeout", cfg.Orders.RequestTimeout.String(),
			"retry_max_attempts", cfg.Orders.Retry.MaxAttempts)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("server stopped")
	return nil
}

func loadAuthenticator(cfg config.Config) (*auth.Authenticator, error) {
	var r io.Reader
	if cfg.APIKeysFile != "" {
		f, err := os.Open(cfg.APIKeysFile)
		if err != nil {
			return nil, fmt.Errorf("open API keys file: %w", err)
		}
		defer f.Close()
		r = f
	} else {
		r = strings.NewReader(cfg.APIKeysJSON)
	}
	entries, err := auth.ParseKeys(r)
	if err != nil {
		return nil, err
	}
	return auth.NewAuthenticator(entries)
}

func newProvider(cfg config.Config) (provider.Provider, error) {
	switch cfg.Provider {
	case "shopify":
		return shopify.New(shopify.Config{
			ShopDomain:            cfg.Shopify.ShopDomain,
			APIVersion:            cfg.Shopify.APIVersion,
			AccessToken:           cfg.Shopify.AccessToken,
			Endpoint:              cfg.Shopify.Endpoint,
			AllowInsecureEndpoint: cfg.Shopify.AllowInsecureEndpoint,
		})
	default:
		return mock.New(fixtures.Orders(),
			mock.WithLatency(cfg.Mock.Latency),
			mock.WithFailureRate(cfg.Mock.FailureRate)), nil
	}
}

// probe performs a local health check; it is used by the container
// HEALTHCHECK because the distroless image has no shell or curl.
func probe(addr string) int {
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/health")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
