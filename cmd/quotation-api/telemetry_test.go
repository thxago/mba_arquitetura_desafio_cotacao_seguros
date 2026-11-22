package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/alicebob/miniredis/v2"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestBusinessTelemetryAtAPIBoot(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	metrics := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	spans := tracetest.NewSpanRecorder()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	oldMetrics, oldTraces := otel.GetMeterProvider(), otel.GetTracerProvider()
	otel.SetMeterProvider(metrics)
	otel.SetTracerProvider(traces)
	t.Cleanup(func() {
		otel.SetMeterProvider(oldMetrics)
		otel.SetTracerProvider(oldTraces)
		_ = metrics.Shutdown(context.Background())
		_ = traces.Shutdown(context.Background())
	})

	store := miniredis.RunT(t)
	var calls atomic.Int32
	var fail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		if fail.Load() {
			http.Error(w, "private-upstream-body", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"quote_id":"private-quote","premium_cents":100,"currency":"BRL","coverage_cents":100000,"valid_for_seconds":300}`))
	}))
	defer upstream.Close()
	values := map[string]string{
		"PARTNER_ENDPOINTS": "partner-flaky=" + upstream.URL, "PARTNER_TIMEOUTS": "partner-flaky=1s",
		"BREAKER_FAILURE_THRESHOLD": "2", "REDIS_ADDR": store.Addr(), "REDIS_OPERATION_TIMEOUT": "100ms",
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
	handler := platform.InstrumentHandler(api, "quotation-api")
	body := `{"driver":{"document":"12345678901","birth_year":1988},"vehicle":{"plate":"ABC1D23","model":"Gol","year":2020,"value_cents":8500000}}`
	for i := 0; i < 6; i++ {
		payload := body
		if i >= 2 {
			fail.Store(true)
			payload = strings.ReplaceAll(body, `"Gol"`, `"Other"`)
		}
		if i == 5 {
			store.Close()
		}
		start := len(spans.Ended())
		req := httptest.NewRequest(http.MethodPost, "/quotes", strings.NewReader(payload))
		req.Header.Set("X-Tenant-Id", "corretora-a")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		wantStatus := http.StatusOK
		if i >= 2 {
			wantStatus = http.StatusServiceUnavailable
		}
		if recorder.Code != wantStatus {
			t.Fatalf("step %d: HTTP %d: %s", i, recorder.Code, recorder.Body)
		}
		if i == 1 || i >= 4 {
			ended := spans.Ended()[start:]
			if len(ended) != 1 || ended[0].SpanKind() != trace.SpanKindServer {
				t.Fatalf("step %d emitted an avoided HTTP span: %v", i, ended)
			}
			event := "quotation.cache_hit"
			if i >= 4 {
				event = "partner.short_circuit"
			}
			found := false
			for _, e := range ended[0].Events() {
				if e.Name == event {
					found = true
					if len(e.Attributes) == 0 || e.Attributes[0].Value.AsString() != "partner-flaky" {
						t.Fatalf("event lost configured partner: %+v", e)
					}
				}
			}
			if !found {
				t.Fatalf("step %d missing %s: %v", i, event, ended[0].Events())
			}
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("cache or short circuit sent HTTP: calls=%d", calls.Load())
	}
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	lookups := make(map[string]int64)
	transitions, durations := int64(0), uint64(0)
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch m.Name {
			case "prumo_cache_lookups":
				for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
					result, _ := point.Attributes.Value("result")
					if point.Attributes.Len() != 2 {
						t.Fatalf("unbounded cache attributes: %v", point.Attributes)
					}
					lookups[result.AsString()] += point.Value
				}
			case "prumo_breaker_transitions":
				for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
					from, _ := point.Attributes.Value("from")
					to, _ := point.Attributes.Value("to")
					if point.Attributes.Len() != 3 || from.AsString() != "closed" || to.AsString() != "open" {
						t.Fatalf("unexpected transition: %+v", point)
					}
					transitions += point.Value
				}
			case "http.client.request.duration":
				for _, point := range m.Data.(metricdata.Histogram[float64]).DataPoints {
					address, ok := point.Attributes.Value("server.address")
					if !ok || address.AsString() != "127.0.0.1" {
						t.Fatalf("latency missing partner endpoint: %+v", point)
					}
					durations += point.Count
				}
			}
		}
	}
	if lookups["hit"] != 1 || lookups["miss"] != 4 || lookups["error"] != 1 || len(lookups) != 3 ||
		transitions != 1 || durations != 3 {
		t.Fatalf("wrong business metrics: lookups=%v transitions=%d HTTP durations=%d", lookups, transitions, durations)
	}
	encodedMetrics, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	telemetry := string(encodedMetrics)
	for _, span := range spans.Ended() {
		telemetry += fmt.Sprint(span.Name(), span.Attributes(), span.Events(), span.Status())
	}
	for _, private := range []string{"12345678901", "ABC1D23", "private-quote", "private-upstream-body", "corretora-a", "prumo:quotes:"} {
		if strings.Contains(telemetry, private) {
			t.Fatalf("private value in telemetry: %s", private)
		}
	}
}