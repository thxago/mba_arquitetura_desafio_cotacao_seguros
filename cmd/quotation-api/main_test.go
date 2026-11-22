package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/quotation"
	"github.com/alicebob/miniredis/v2"
)

func TestBootWiresSharedPartnerBreakerIntoQuoteRoute(t *testing.T) {
	redis := miniredis.RunT(t)
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
		"REDIS_ADDR":                redis.Addr(),
		"REDIS_OPERATION_TIMEOUT":   "1s",
	}
	cfg, err := platform.LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	api, closeCache, err := newAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeCache() })
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
	if _, _, err := newAPI(cfg); err == nil {
		t.Fatal("boot accepted invalid breaker configuration")
	}
}

func TestBootCachesPerTenantAndPartnerBeforeBreakerAndSurvivesRedisOutage(t *testing.T) {
	redisServer := miniredis.RunT(t)
	var calls atomic.Int32
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		var request struct {
			Broker string `json:"broker"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		price := 100
		if request.Broker == "corretora-b" {
			price = 200
		}
		if strings.HasPrefix(r.URL.Path, "/second/") {
			price += 10
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"quote_id":      fmt.Sprintf("%s-%s", r.URL.Path, request.Broker),
			"premium_cents": price, "currency": "BRL", "coverage_cents": 100000, "valid_for_seconds": 300,
		})
	}))
	defer upstream.Close()
	values := map[string]string{
		"PARTNER_ENDPOINTS":         "first=" + upstream.URL + "/first,second=" + upstream.URL + "/second",
		"PARTNER_TIMEOUTS":          "first=1s,second=1s",
		"BREAKER_FAILURE_THRESHOLD": "2",
		"OTEL_SDK_DISABLED":         "true",
		"REDIS_ADDR":                redisServer.Addr(),
		"REDIS_OPERATION_TIMEOUT":   "200ms",
	}
	cfg, err := platform.LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	api, closeCache, err := newAPI(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeCache() })
	body := `{"driver":{"document":"12345678901","birth_year":1988},"vehicle":{"plate":"ABC1D23","model":"Gol","year":2020,"value_cents":8500000}}`
	post := func(handler http.Handler, tenant, payload string, status int) quotation.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/quotes", strings.NewReader(payload))
		req.Header.Set("X-Tenant-Id", tenant)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		if recorder.Code != status {
			t.Fatalf("unexpected status %d, want %d: %s", recorder.Code, status, recorder.Body)
		}
		var response quotation.Response
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	first := post(api, "corretora-a", body, http.StatusOK)
	normalizedVariant := strings.ReplaceAll(strings.ReplaceAll(body, "ABC1D23", " abc1d23 "), `"Gol"`, `" Gol "`)
	hit := post(api, "corretora-a", normalizedVariant, http.StatusOK)
	second := post(api, "corretora-b", body, http.StatusOK)
	if calls.Load() != 4 || len(redisServer.Keys()) != 4 || len(hit.Quotes) != 2 || len(second.Quotes) != 2 {
		t.Fatalf("cache not partitioned by tenant and partner: calls=%d keys=%d", calls.Load(), len(redisServer.Keys()))
	}
	for i, quote := range hit.Quotes {
		if quote.Source != "cache" || quote.QuoteID != first.Quotes[i].QuoteID ||
			!quote.QuotedAt.Equal(first.Quotes[i].QuotedAt) || !quote.ExpiresAt.Equal(first.Quotes[i].ExpiresAt) ||
			quote.ValidForSeconds >= 300 || quote.ValidForSeconds < 1 ||
			quote.PremiumCents == second.Quotes[i].PremiumCents || quote.QuoteID == second.Quotes[i].QuoteID {
			t.Fatalf("quote lost origin, freshness or isolation: %+v", quote)
		}
	}
	fail.Store(true)
	differentRisk := strings.ReplaceAll(body, `"Gol"`, `"Other"`)
	for i := 0; i < 2; i++ {
		post(api, "corretora-a", differentRisk, http.StatusBadGateway)
	}
	_ = post(api, "corretora-a", body, http.StatusOK)
	if calls.Load() != 6 {
		t.Fatalf("valid cache hit reached open breaker/upstream: calls=%d", calls.Load())
	}

	fail.Store(false)
	redisServer.Close()
	withoutRedis, closeWithoutRedis, err := newAPI(cfg)
	if err != nil {
		t.Fatalf("Redis outage should not prevent boot: %v", err)
	}
	t.Cleanup(func() { _ = closeWithoutRedis() })
	degraded := post(withoutRedis, "corretora-a", body, http.StatusOK)
	if degraded.CacheStatus != "unavailable" || !degraded.Degraded || len(degraded.Quotes) != 2 {
		t.Fatalf("cache outage not explicit or fresh proposals lost: %+v", degraded)
	}
	for _, quote := range degraded.Quotes {
		if quote.Source != "partner" {
			t.Fatalf("cache outage fabricated a cache hit: %+v", quote)
		}
	}
}