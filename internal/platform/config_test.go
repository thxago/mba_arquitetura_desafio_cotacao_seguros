package platform

import (
	"reflect"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestDefaultConfigPointsAtTheComposeEnvironment(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("port %q, want 8080", cfg.Port)
	}
	want := []Partner{
		{Name: "partner-slow", BaseURL: "http://localhost:9001"},
		{Name: "partner-flaky", BaseURL: "http://localhost:9002"},
		{Name: "partner-degrading", BaseURL: "http://localhost:9003"},
	}
	if !reflect.DeepEqual(cfg.Partners, want) {
		t.Errorf("partners %+v, want %+v", cfg.Partners, want)
	}
	if !reflect.DeepEqual(cfg.Tenants, []string{"corretora-a", "corretora-b"}) {
		t.Errorf("brokers %v, want corretora-a and corretora-b", cfg.Tenants)
	}
}

func TestConfigReadsPartnersAndTenantsFromTheEnvironment(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"PORT":              "9090",
		"PARTNER_ENDPOINTS": " a=http://a:8080/ , b=http://b:8080 ",
		"PARTNER_TIMEOUTS":  " a=2s , b=500ms ",
		"TENANTS":           " corretora-x , corretora-y ",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := []Partner{{Name: "a", BaseURL: "http://a:8080"}, {Name: "b", BaseURL: "http://b:8080"}}
	if !reflect.DeepEqual(cfg.Partners, want) {
		t.Errorf("partners %+v, want %+v", cfg.Partners, want)
	}
	if !reflect.DeepEqual(cfg.Tenants, []string{"corretora-x", "corretora-y"}) {
		t.Errorf("unexpected brokers %v", cfg.Tenants)
	}
	if cfg.Port != "9090" {
		t.Errorf("port %q, want 9090", cfg.Port)
	}
}

func TestResilienceConfiguration(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Resilience.BreakerFailureThreshold != 3 || cfg.Resilience.BreakerMaxRequests != 1 ||
		cfg.Resilience.BreakerInterval != 10*time.Second || cfg.Resilience.BreakerOpenTimeout != 5*time.Second {
		t.Fatalf("unexpected breaker policy: %+v", cfg.Resilience)
	}
	want := map[string]time.Duration{
		"partner-slow": 2 * time.Second, "partner-flaky": 500 * time.Millisecond, "partner-degrading": time.Second,
	}
	if !reflect.DeepEqual(cfg.Resilience.PartnerTimeouts, want) {
		t.Fatalf("timeouts %v, want %v", cfg.Resilience.PartnerTimeouts, want)
	}

	cfg, err = LoadConfig(env(map[string]string{
		"PARTNER_ENDPOINTS":         "a=http://a:8080",
		"PARTNER_TIMEOUTS":          "a=750ms",
		"BREAKER_FAILURE_THRESHOLD": "4",
		"BREAKER_MAX_REQUESTS":      "2",
		"BREAKER_INTERVAL":          "12s",
		"BREAKER_OPEN_TIMEOUT":      "7s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Resilience
	if r.PartnerTimeouts["a"] != 750*time.Millisecond || r.BreakerFailureThreshold != 4 ||
		r.BreakerMaxRequests != 2 || r.BreakerInterval != 12*time.Second || r.BreakerOpenTimeout != 7*time.Second {
		t.Fatalf("environment policy not applied: %+v", r)
	}
}

func TestInvalidResilienceConfigurationFails(t *testing.T) {
	cases := map[string]map[string]string{
		"missing timeout":    {"PARTNER_TIMEOUTS": "partner-slow=2s"},
		"unknown partner":    {"PARTNER_TIMEOUTS": defaultTimeouts + ",other=1s"},
		"repeated timeout":   {"PARTNER_TIMEOUTS": defaultTimeouts + ",partner-slow=1s"},
		"zero timeout":       {"PARTNER_TIMEOUTS": "partner-slow=0s"},
		"negative timeout":   {"PARTNER_TIMEOUTS": "partner-slow=-1s"},
		"invalid timeout":    {"PARTNER_TIMEOUTS": "partner-slow=soon"},
		"missing name":       {"PARTNER_TIMEOUTS": "=1s"},
		"missing separator":  {"PARTNER_TIMEOUTS": "partner-slow"},
		"empty timeouts":     {"PARTNER_TIMEOUTS": " "},
		"new partner":        {"PARTNER_ENDPOINTS": "a=http://a:8080"},
		"unsupported scheme": {"PARTNER_ENDPOINTS": "partner-slow=ftp://a:8080"},
	}
	for _, key := range []string{"BREAKER_FAILURE_THRESHOLD", "BREAKER_MAX_REQUESTS"} {
		for _, value := range []string{"0", "-1", "three", "4294967296"} {
			cases[key+"="+value] = map[string]string{key: value}
		}
	}
	for _, key := range []string{"BREAKER_INTERVAL", "BREAKER_OPEN_TIMEOUT"} {
		for _, value := range []string{"0s", "-1s", "soon", "999999999999999999999s"} {
			cases[key+"="+value] = map[string]string{key: value}
		}
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(env(values)); err == nil {
				t.Fatalf("invalid resilience configuration accepted: %v", values)
			}
		})
	}
}

func TestCacheConfigDefaultsAndOverrides(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Cache.Address != "127.0.0.1:6379" || cfg.Cache.TTL != 300*time.Second ||
		cfg.Cache.OperationTimeout != 20*time.Millisecond {
		t.Fatalf("unexpected cache defaults: %+v", cfg.Cache)
	}
	cfg, err = LoadConfig(env(map[string]string{
		"REDIS_ADDR": " [::1]:6380 ", "CACHE_TTL": "45s", "REDIS_OPERATION_TIMEOUT": "15ms",
	}))
	if err != nil || cfg.Cache.Address != "[::1]:6380" || cfg.Cache.TTL != 45*time.Second ||
		cfg.Cache.OperationTimeout != 15*time.Millisecond {
		t.Fatalf("cache overrides: %+v err=%v", cfg.Cache, err)
	}
}

func TestInvalidCacheConfigurationFails(t *testing.T) {
	for key, values := range map[string][]string{
		"REDIS_ADDR":              {" ", "redis", ":6379", "redis:0", "redis:65536", "redis:port", "redis://host:6379", "bad host:6379"},
		"CACHE_TTL":               {"0s", "-1s", "301s", "24h", "soon", "999999999999999999999s"},
		"REDIS_OPERATION_TIMEOUT": {"0s", "-1s", "soon"},
	} {
		for _, value := range values {
			t.Run(key+"="+value, func(t *testing.T) {
				if _, err := LoadConfig(env(map[string]string{key: value})); err == nil {
					t.Fatal("invalid cache configuration accepted")
				}
			})
		}
	}
}

func TestTelemetryIsOnByDefault(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := Telemetry{ServiceName: "quotation-api", Endpoint: "http://localhost:4317", Enabled: true}
	if cfg.Telemetry != want {
		t.Errorf("telemetry %+v, want %+v", cfg.Telemetry, want)
	}
}

func TestTelemetryReadsTheStandardOTelVariables(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"OTEL_SERVICE_NAME":           "quotation-api-poc",
		"OTEL_EXPORTER_OTLP_ENDPOINT": " http://otel-collector:4317 ",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := Telemetry{ServiceName: "quotation-api-poc", Endpoint: "http://otel-collector:4317", Enabled: true}
	if cfg.Telemetry != want {
		t.Errorf("telemetry %+v, want %+v", cfg.Telemetry, want)
	}
}

func TestTelemetryCanBeTurnedOff(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{"OTEL_SDK_DISABLED": "TRUE"}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Telemetry.Enabled {
		t.Error("OTEL_SDK_DISABLED=TRUE did not turn the SDK off")
	}
}

func TestTelemetryOffSkipsEndpointValidation(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{
		"OTEL_SDK_DISABLED":           "true",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "not-a-url",
	})); err != nil {
		t.Fatalf("LoadConfig with the SDK off: %v", err)
	}
}

func TestInvalidConfigFails(t *testing.T) {
	cases := map[string]map[string]string{
		"partner without url":                {"PARTNER_ENDPOINTS": "partner-slow"},
		"relative url":                       {"PARTNER_ENDPOINTS": "partner-slow=/quotes"},
		"url without host":                   {"PARTNER_ENDPOINTS": "partner-slow=http://"},
		"partner without name":               {"PARTNER_ENDPOINTS": "=http://a:8080"},
		"repeated partner":                   {"PARTNER_ENDPOINTS": "a=http://a:8080,a=http://b:8080"},
		"empty partner list":                 {"PARTNER_ENDPOINTS": " , "},
		"empty tenant list":                  {"TENANTS": " , "},
		"collector without scheme":           {"OTEL_EXPORTER_OTLP_ENDPOINT": "otel-collector:4317"},
		"collector without host":             {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://"},
		"OTEL_SDK_DISABLED is not a boolean": {"OTEL_SDK_DISABLED": "maybe"},
	}

	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(env(vars)); err == nil {
				t.Fatalf("invalid configuration (%v) was accepted", vars)
			}
		})
	}
}