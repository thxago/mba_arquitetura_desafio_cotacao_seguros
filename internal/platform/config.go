package platform

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port       string
	Partners   []Partner
	Tenants    []string
	Telemetry  Telemetry
	Resilience Resilience
}

type Resilience struct {
	PartnerTimeouts         map[string]time.Duration
	BreakerFailureThreshold uint32
	BreakerInterval         time.Duration
	BreakerOpenTimeout      time.Duration
	BreakerMaxRequests      uint32
}

type Partner struct {
	Name    string
	BaseURL string
}

type Telemetry struct {
	ServiceName string
	Endpoint    string
	Enabled     bool
}

const defaultEndpoints = "partner-slow=http://localhost:9001," +
	"partner-flaky=http://localhost:9002," +
	"partner-degrading=http://localhost:9003"

const defaultTenants = "corretora-a,corretora-b"

const defaultCollector = "http://localhost:4317"

const defaultServiceName = "quotation-api"

const defaultTimeouts = "partner-slow=2s,partner-flaky=500ms,partner-degrading=1s"

func LoadConfig(env func(string) string) (Config, error) {
	cfg := Config{Port: text(env, "PORT", "8080")}

	var err error
	if cfg.Partners, err = parsePartners(text(env, "PARTNER_ENDPOINTS", defaultEndpoints)); err != nil {
		return Config{}, err
	}
	if cfg.Tenants, err = parseTenants(text(env, "TENANTS", defaultTenants)); err != nil {
		return Config{}, err
	}
	if cfg.Telemetry, err = parseTelemetry(env); err != nil {
		return Config{}, err
	}
	if cfg.Resilience, err = parseResilience(env, cfg.Partners); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseResilience(env func(string) string, partners []Partner) (Resilience, error) {
	cfg := Resilience{PartnerTimeouts: make(map[string]time.Duration)}
	for _, item := range strings.Split(text(env, "PARTNER_TIMEOUTS", defaultTimeouts), ",") {
		name, value, found := strings.Cut(strings.TrimSpace(item), "=")
		name = strings.TrimSpace(name)
		duration, err := time.ParseDuration(strings.TrimSpace(value))
		if !found || name == "" || err != nil || duration <= 0 {
			return Resilience{}, fmt.Errorf("PARTNER_TIMEOUTS: %q must be name=positive-duration", item)
		}
		if _, exists := cfg.PartnerTimeouts[name]; exists {
			return Resilience{}, fmt.Errorf("PARTNER_TIMEOUTS: partner %q is repeated", name)
		}
		cfg.PartnerTimeouts[name] = duration
	}

	for _, setting := range []struct {
		name     string
		fallback string
		target   *uint32
	}{
		{"BREAKER_FAILURE_THRESHOLD", "3", &cfg.BreakerFailureThreshold},
		{"BREAKER_MAX_REQUESTS", "1", &cfg.BreakerMaxRequests},
	} {
		value, err := strconv.ParseUint(strings.TrimSpace(text(env, setting.name, setting.fallback)), 10, 32)
		if err != nil || value == 0 {
			return Resilience{}, fmt.Errorf("%s: must be a positive uint32", setting.name)
		}
		*setting.target = uint32(value)
	}
	for _, setting := range []struct {
		name     string
		fallback string
		target   *time.Duration
	}{
		{"BREAKER_INTERVAL", "10s", &cfg.BreakerInterval},
		{"BREAKER_OPEN_TIMEOUT", "5s", &cfg.BreakerOpenTimeout},
	} {
		value, err := time.ParseDuration(strings.TrimSpace(text(env, setting.name, setting.fallback)))
		if err != nil || value <= 0 {
			return Resilience{}, fmt.Errorf("%s: must be a positive duration", setting.name)
		}
		*setting.target = value
	}
	return cfg, cfg.Validate(partners)
}

func (cfg Resilience) Validate(partners []Partner) error {
	if cfg.BreakerFailureThreshold == 0 || cfg.BreakerMaxRequests == 0 ||
		cfg.BreakerInterval <= 0 || cfg.BreakerOpenTimeout <= 0 {
		return fmt.Errorf("breaker: threshold, max requests, interval and open timeout must be positive")
	}
	if len(partners) == 0 {
		return fmt.Errorf("resilience: at least one partner is required")
	}
	seen := make(map[string]bool, len(partners))
	for _, p := range partners {
		u, err := url.Parse(p.BaseURL)
		if p.Name == "" || seen[p.Name] || err != nil || u.Host == "" ||
			(u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("resilience: invalid or repeated partner %q", p.Name)
		}
		seen[p.Name] = true
		if cfg.PartnerTimeouts[p.Name] <= 0 {
			return fmt.Errorf("PARTNER_TIMEOUTS: missing positive timeout for %q", p.Name)
		}
	}
	for name := range cfg.PartnerTimeouts {
		if !seen[name] {
			return fmt.Errorf("PARTNER_TIMEOUTS: unknown partner %q", name)
		}
	}
	return nil
}

func parseTelemetry(env func(string) string) (Telemetry, error) {
	telemetry := Telemetry{
		ServiceName: text(env, "OTEL_SERVICE_NAME", defaultServiceName),
		Endpoint:    strings.TrimSpace(text(env, "OTEL_EXPORTER_OTLP_ENDPOINT", defaultCollector)),
	}

	switch disabled := strings.ToLower(strings.TrimSpace(env("OTEL_SDK_DISABLED"))); disabled {
	case "", "false":
		telemetry.Enabled = true
	case "true":
		return Telemetry{Enabled: false}, nil
	default:
		return Telemetry{}, fmt.Errorf("OTEL_SDK_DISABLED: %q is neither true nor false", disabled)
	}

	if u, err := url.Parse(telemetry.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return Telemetry{}, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT: %q is not an absolute URL", telemetry.Endpoint)
	}
	return telemetry, nil
}

func parsePartners(raw string) ([]Partner, error) {
	var partners []Partner
	seen := map[string]bool{}

	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		name, endpoint, found := strings.Cut(item, "=")
		name, endpoint = strings.TrimSpace(name), strings.TrimSpace(endpoint)
		if !found || name == "" || endpoint == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not in the name=url format", item)
		}

		if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not an absolute URL", endpoint)
		}
		if seen[name] {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: partner %q is repeated", name)
		}

		seen[name] = true
		partners = append(partners, Partner{Name: name, BaseURL: strings.TrimRight(endpoint, "/")})
	}

	if len(partners) == 0 {
		return nil, fmt.Errorf("PARTNER_ENDPOINTS: at least one partner is required")
	}
	return partners, nil
}

func parseTenants(raw string) ([]string, error) {
	var tenants []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tenants = append(tenants, t)
		}
	}
	if len(tenants) == 0 {
		return nil, fmt.Errorf("TENANTS: at least one broker is required")
	}
	return tenants, nil
}

func Environment() func(string) string { return os.Getenv }

func text(env func(string) string, key, fallback string) string {
	if v := env(key); v != "" {
		return v
	}
	return fallback
}