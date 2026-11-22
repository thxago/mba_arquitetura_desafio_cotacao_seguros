package quotation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

const validBody = `{
  "driver": {"document": "12345678901", "birth_year": 1988},
  "vehicle": {"plate": "abc1d23", "model": "Gol 1.0", "year": 2020, "value_cents": 8500000},
  "coverage": "comprehensive"
}`

func testAPI(quoter Quoter) http.Handler {
	return NewAPI(NewService(threePartners, quoter), []string{"corretora-a", "corretora-b"}).Routes()
}

func postQuotes(h http.Handler, tenant, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/quotes", strings.NewReader(body))
	if tenant != "" {
		req.Header.Set(TenantHeader, tenant)
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	return response
}

func TestQuotesReturnsAggregatedQuotes(t *testing.T) {
	response := postQuotes(testAPI(&fakeQuoter{premiums: defaultPremiums()}), "corretora-a", validBody)
	if response.Code != http.StatusOK {
		t.Fatalf("status %d, expected 200: %s", response.Code, response.Body)
	}

	var body Response
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable response: %v", err)
	}
	if len(body.Quotes) != 3 {
		t.Fatalf("%d quotes, expected 3", len(body.Quotes))
	}
	if body.TenantID != "corretora-a" {
		t.Errorf("tenant_id %q, expected corretora-a", body.TenantID)
	}
}

func TestQuotesRejectsRequestWithoutTenant(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums()}
	response := postQuotes(testAPI(quoter), "", validBody)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status %d, expected 400", response.Code)
	}
	if len(quoter.calls) != 0 {
		t.Fatalf("the partners were called (%v) even without a tenant", quoter.calls)
	}
}

func TestQuotesRejectsUnknownBroker(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums()}
	response := postQuotes(testAPI(quoter), "corretora-pirata", validBody)

	if response.Code != http.StatusForbidden {
		t.Fatalf("status %d, expected 403", response.Code)
	}
	if len(quoter.calls) != 0 {
		t.Fatalf("the partners were called (%v) for a broker that is not enabled", quoter.calls)
	}
}

func TestQuotesRejectsInvalidBody(t *testing.T) {
	cases := map[string]string{
		"broken json":      `{"driver":`,
		"missing document": `{"driver":{"birth_year":1988},"vehicle":{"plate":"ABC1D23","year":2020,"value_cents":100}}`,
		"missing plate":    `{"driver":{"document":"1","birth_year":1988},"vehicle":{"year":2020,"value_cents":100}}`,
		"zero value":       `{"driver":{"document":"1","birth_year":1988},"vehicle":{"plate":"A","year":2020,"value_cents":0}}`,
		"invalid coverage": `{"driver":{"document":"1","birth_year":1988},"vehicle":{"plate":"A","year":2020,"value_cents":1},"coverage":"vip"}`,
		"unknown field":    `{"driver":{"document":"1","birth_year":1988},"vehicle":{"plate":"A","year":2020,"value_cents":1},"discount":true}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			response := postQuotes(testAPI(&fakeQuoter{premiums: defaultPremiums()}), "corretora-a", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status %d, expected 400: %s", response.Code, response.Body)
			}
		})
	}
}

func TestQuotesResponds502WithTheNameOfThePartnerThatWentDown(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums(), failOn: "partner-flaky"}
	response := postQuotes(testAPI(quoter), "corretora-a", validBody)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status %d, expected 502", response.Code)
	}

	var body platform.ErrorBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable error: %v", err)
	}
	if body.Partner != "partner-flaky" {
		t.Errorf("partner blamed %q, expected partner-flaky", body.Partner)
	}
}

func TestHealthz(t *testing.T) {
	response := httptest.NewRecorder()
	testAPI(&fakeQuoter{premiums: defaultPremiums()}).
		ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status %d, expected 200", response.Code)
	}
}

func TestResilientHTTPContract(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		failed     int
		redisError bool
		status     string
		code       int
	}{
		{"complete", 0, false, statusComplete, http.StatusOK},
		{"partial", 1, false, statusPartial, http.StatusOK},
		{"unavailable", 3, false, statusUnavailable, http.StatusServiceUnavailable},
		{"complete without Redis", 0, true, statusComplete, http.StatusOK},
		{"partial without Redis", 1, true, statusPartial, http.StatusOK},
		{"unavailable without Redis", 3, true, statusUnavailable, http.StatusServiceUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			store := newMemoryCache()
			if scenario.redisError {
				store.getErr, store.setErr = errors.New("GET failed"), errors.New("SET failed")
			}
			service := cachedService(t, threePartners, cacheQuoterFunc(func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
				calls++
				if calls <= scenario.failed {
					return partner.Quote{}, &partner.Error{Partner: p.Name, Kind: "upstream_error", Reason: "private details"}
				}
				return originalQuote(p, 300), nil
			}), store, 300*time.Second)
			api := NewAPI(service, []string{"corretora-a"}).Routes()
			recorder := postQuotes(api, "corretora-a", validBody)
			if recorder.Code != scenario.code {
				t.Fatalf("HTTP %d, want %d: %s", recorder.Code, scenario.code, recorder.Body)
			}
			var body Response
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			cacheStatus := "available"
			if scenario.redisError {
				cacheStatus = "unavailable"
			}
			if body.Status != scenario.status || body.Degraded != (scenario.failed > 0 || scenario.redisError) ||
				body.CacheStatus != cacheStatus || len(body.Quotes) != 3-scenario.failed ||
				len(body.UnavailablePartners) != scenario.failed || calls != 3 || body.TenantID != "corretora-a" {
				t.Fatalf("contract lost completeness, cache state or candidates: %+v calls=%d", body, calls)
			}
			for i, missing := range body.UnavailablePartners {
				if missing.Partner != threePartners[i].Name || missing.Reason != "upstream_error" {
					t.Fatalf("wrong unavailable partner/reason: %+v", missing)
				}
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(recorder.Body.Bytes(), &fields)
			if string(fields["quotes"]) == "null" || string(fields["unavailable_partners"]) == "null" ||
				len(fields["quotes"]) == 0 || len(fields["unavailable_partners"]) == 0 {
				t.Fatalf("contract arrays must be present, including empty arrays: %s", recorder.Body)
			}
			if scenario.code == http.StatusOK {
				if recorder.Header().Get(TenantHeader) != "corretora-a" || fields["error"] != nil || fields["action"] != nil {
					t.Fatalf("successful quote lost tenant header or carried refusal fields: %s", recorder.Body)
				}
			} else if body.Error != "no_valid_quotes" ||
				body.Action != "Solicite uma nova cotação mais tarde ou contate o suporte da corretora." ||
				string(fields["quotes"]) != "[]" {
				t.Fatalf("empty response lacks business refusal and orientation: %s", recorder.Body)
			}
		})
	}
}

func TestResilientRoutesPreserveValidationAndCoverage(t *testing.T) {
	for _, scenario := range []struct {
		name, tenant, body, coverage string
		code                         int
	}{
		{"missing tenant", "", validBody, "", http.StatusBadRequest},
		{"unknown tenant", "other", validBody, "", http.StatusForbidden},
		{"invalid JSON", "corretora-a", "{", "", http.StatusBadRequest},
		{"unknown field", "corretora-a", strings.Replace(validBody, `"driver"`, `"unknown"`, 1), "", http.StatusBadRequest},
		{"missing document", "corretora-a", strings.Replace(validBody, `"12345678901"`, `""`, 1), "", http.StatusBadRequest},
		{"invalid coverage", "corretora-a", strings.Replace(validBody, "comprehensive", "vip", 1), "", http.StatusBadRequest},
		{"comprehensive", "corretora-a", validBody, "comprehensive", http.StatusOK},
		{"third party", "corretora-a", strings.Replace(validBody, "comprehensive", "third_party", 1), "third_party", http.StatusOK},
		{"default coverage", "corretora-a", strings.Replace(validBody, "comprehensive", "", 1), "comprehensive", http.StatusOK},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			store := newMemoryCache()
			service := cachedService(t, threePartners, cacheQuoterFunc(func(_ context.Context, p platform.Partner, raw any) (partner.Quote, error) {
				calls++
				request := raw.(partnerRequest)
				if request.Broker != scenario.tenant || request.Coverage != scenario.coverage || request.Vehicle.Plate != "ABC1D23" {
					t.Fatalf("tenant/normalized risk not preserved: %+v", request)
				}
				return originalQuote(p, 300), nil
			}), store, 300*time.Second)
			api := NewAPI(service, []string{"corretora-a"}).Routes()
			recorder := postQuotes(api, scenario.tenant, scenario.body)
			if recorder.Code != scenario.code {
				t.Fatalf("HTTP %d, want %d: %s", recorder.Code, scenario.code, recorder.Body)
			}
			if scenario.code != http.StatusOK && (calls != 0 || store.gets != 0 || store.sets != 0) {
				t.Fatal("invalid request reached cache/partners")
			}
			health := httptest.NewRecorder()
			api.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if health.Code != http.StatusOK {
				t.Fatalf("health route changed: %d", health.Code)
			}
		})
	}
}