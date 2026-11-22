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
		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("unexpected failure response: %d %s", recorder.Code, recorder.Body)
		}
		var response quotation.Response
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		reason := "upstream_error"
		if i >= 2 {
			reason = "circuit_open"
		}
		if response.Status != "unavailable" || response.Error != "no_valid_quotes" ||
			len(response.Quotes) != 0 || len(response.UnavailablePartners) != 1 ||
			response.UnavailablePartners[0] != (quotation.UnavailablePartner{Partner: "partner-flaky", Reason: reason}) {
			t.Fatalf("unavailable contract lost breaker reason: %+v", response)
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
		post(api, "corretora-a", differentRisk, http.StatusServiceUnavailable)
	}
	cachedWithOpenBreakers := post(api, "corretora-a", body, http.StatusOK)
	if calls.Load() != 8 || cachedWithOpenBreakers.Status != "complete" || cachedWithOpenBreakers.Degraded ||
		len(cachedWithOpenBreakers.UnavailablePartners) != 0 || cachedWithOpenBreakers.CacheStatus != "available" {
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
	if degraded.CacheStatus != "unavailable" || !degraded.Degraded || len(degraded.Quotes) != 2 ||
		degraded.Status != "complete" || len(degraded.UnavailablePartners) != 0 {
		t.Fatalf("cache outage not explicit or fresh proposals lost: %+v", degraded)
	}
	for _, quote := range degraded.Quotes {
		if quote.Source != "partner" {
			t.Fatalf("cache outage fabricated a cache hit: %+v", quote)
		}
	}
}

func TestBootReturnsPartialQuotesAndExplicitRefusal(t *testing.T) {
	redisServer := miniredis.RunT(t)
	var calls atomic.Int32
	var failAll atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if failAll.Load() || strings.HasPrefix(r.URL.Path, "/flaky/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		price := 200
		if strings.HasPrefix(r.URL.Path, "/degrading/") {
			price = 100
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"quote_id": r.URL.Path, "premium_cents": price, "currency": "BRL",
			"coverage_cents": 100000, "valid_for_seconds": 300,
		})
	}))
	defer upstream.Close()
	values := map[string]string{
		"PARTNER_ENDPOINTS": "partner-slow=" + upstream.URL + "/slow,partner-flaky=" + upstream.URL +
			"/flaky,partner-degrading=" + upstream.URL + "/degrading",
		"BREAKER_FAILURE_THRESHOLD": "2",
		"OTEL_SDK_DISABLED":         "true",
		"REDIS_ADDR":                redisServer.Addr(),
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
	for i := 0; i < 4; i++ {
		payload := body
		wantCode, wantStatus, wantQuotes := http.StatusOK, "partial", 2
		if i == 3 {
			failAll.Store(true)
			payload = strings.ReplaceAll(body, `"Gol"`, `"New risk"`)
			wantCode, wantStatus, wantQuotes = http.StatusServiceUnavailable, "unavailable", 0
		}
		req := httptest.NewRequest(http.MethodPost, "/quotes", strings.NewReader(payload))
		req.Header.Set("X-Tenant-Id", "corretora-a")
		recorder := httptest.NewRecorder()
		api.ServeHTTP(recorder, req)
		var response quotation.Response
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != wantCode || response.Status != wantStatus || !response.Degraded ||
			response.CacheStatus != "available" || len(response.Quotes) != wantQuotes {
			t.Fatalf("API boot did not use partial/refusal contract: HTTP=%d response=%+v", recorder.Code, response)
		}
		if i < 3 {
			reason, source := "upstream_error", "partner"
			if i >= 1 {
				source = "cache"
			}
			if i == 2 {
				reason = "circuit_open"
			}
			if len(response.UnavailablePartners) != 1 ||
				response.UnavailablePartners[0] != (quotation.UnavailablePartner{Partner: "partner-flaky", Reason: reason}) ||
				recorder.Header().Get("X-Tenant-Id") != "corretora-a" {
				t.Fatalf("partial quote lost missing partner/tenant: %+v", response)
			}
			for j, quote := range response.Quotes {
				name, path, price := "partner-degrading", "/degrading/quotes", int64(100)
				if j == 1 {
					name, path, price = "partner-slow", "/slow/quotes", 200
				}
				if quote.Partner != name || quote.QuoteID != path || quote.PremiumCents != price ||
					quote.Source != source || quote.ValidForSeconds < 1 || quote.QuotedAt.IsZero() || quote.ExpiresAt.IsZero() {
					t.Fatalf("partial quote lost price/order/origin: %+v", quote)
				}
			}
		} else if response.Error != "no_valid_quotes" || response.Action == "" || len(response.UnavailablePartners) != 3 {
			t.Fatalf("empty quote was not refused with guidance: %+v", response)
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("aggregation aborted, bypassed cache/breaker or retried: calls=%d want 6", calls.Load())
	}
}