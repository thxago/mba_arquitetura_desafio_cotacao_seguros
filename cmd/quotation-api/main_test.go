package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

func TestBootWiresSharedPartnerBreakerIntoQuoteRoute(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	values := map[string]string{
		"PARTNER_ENDPOINTS":         "partner-flaky=" + upstream.URL,
		"PARTNER_TIMEOUTS":          "partner-flaky=500ms",
		"BREAKER_FAILURE_THRESHOLD": "2",
		"OTEL_SDK_DISABLED":         "true",
	}
	cfg, err := platform.LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	api, err := newAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"driver":{"document":"12345678901","birth_year":1988},"vehicle":{"plate":"ABC1D23","model":"Gol","year":2020,"value_cents":8500000}}`
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/quotes", strings.NewReader(body))
		req.Header.Set("X-Tenant-Id", []string{"corretora-a", "corretora-b"}[i%2])
		recorder := httptest.NewRecorder()
		api.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "partner-flaky") {
			t.Fatalf("unexpected failure response: %d %s", recorder.Code, recorder.Body)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("API boot bypassed configured breaker: upstream received %d calls, want 2", calls.Load())
	}
}

func TestBootDoesNotSilentlyDisableInvalidProtection(t *testing.T) {
	cfg, err := platform.LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resilience.BreakerFailureThreshold = 0
	if _, err := newAPI(cfg); err == nil {
		t.Fatal("boot accepted invalid breaker configuration")
	}
}