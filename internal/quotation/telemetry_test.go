package quotation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestCacheTelemetryClassifiesLookupsAndWriteFailures(t *testing.T) {
	for _, test := range []struct {
		name, result, operation string
	}{
		{"empty", "miss", ""},
		{"valid", "hit", ""},
		{"malformed", "miss", ""},
		{"expired", "miss", ""},
		{"get_failure", "error", "get"},
		{"set_failure", "miss", "set"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			metrics := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			spans := tracetest.NewSpanRecorder()
			traces := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
			oldMetrics, oldTraces := otel.GetMeterProvider(), otel.GetTracerProvider()
			otel.SetMeterProvider(metrics)
			otel.SetTracerProvider(traces)
			defer func() {
				otel.SetMeterProvider(oldMetrics)
				otel.SetTracerProvider(oldTraces)
				_ = metrics.Shutdown(context.Background())
				_ = traces.Shutdown(context.Background())
			}()
			p := threePartners[0]
			store := newMemoryCache()
			key, _ := cacheKey("corretora-a", p.Name, validRequest())
			switch test.name {
			case "valid", "expired":
				now := time.Now().UTC()
				if test.name == "expired" {
					now = now.Add(-301 * time.Second)
				}
				store.values[key], _ = json.Marshal(cacheEntry{
					Version: 1, TenantID: "corretora-a", Quote: originalQuote(p, 300),
					QuotedAt: now, ExpiresAt: now.Add(300 * time.Second),
				})
			case "malformed":
				store.values[key] = []byte("{")
			case "get_failure":
				store.getErr = errors.New("private Redis error")
			case "set_failure":
				store.setErr = errors.New("private Redis error")
			}
			service := cachedService(t, []platform.Partner{p}, cacheQuoterFunc(
				func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
					return originalQuote(p, 300), nil
				}), store, 300*time.Second)
			ctx, span := traces.Tracer("test").Start(context.Background(), "quotation")
			response, err := service.Quote(ctx, "corretora-a", validRequest())
			span.End()
			if err != nil || len(response.Quotes) != 1 {
				t.Fatalf("telemetry changed business result: %+v %v", response, err)
			}
			var data metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &data); err != nil {
				t.Fatal(err)
			}
			points := 0
			for _, scope := range data.ScopeMetrics {
				for _, m := range scope.Metrics {
					if m.Name != "prumo_cache_lookups" {
						continue
					}
					for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
						points++
						result, _ := point.Attributes.Value("result")
						if point.Value != 1 || result.AsString() != test.result {
							t.Fatalf("wrong lookup classification: %+v", point)
						}
					}
				}
			}
			if points != 1 {
				t.Fatalf("want exactly one lookup observation, got %d", points)
			}
			hits, failures := 0, 0
			for _, ended := range spans.Ended() {
				for _, event := range ended.Events() {
					switch event.Name {
					case "quotation.cache_hit":
						hits++
					case "quotation.cache_error":
						failures++
						if len(event.Attributes) != 2 || event.Attributes[1].Value.AsString() != test.operation {
							t.Fatalf("wrong cache error event: %+v", event)
						}
					}
				}
			}
			wantHits, wantFailures := 0, 0
			if test.result == "hit" {
				wantHits = 1
			}
			if test.operation != "" {
				wantFailures = 1
			}
			if hits != wantHits || failures != wantFailures {
				t.Fatalf("wrong cache events: hits=%d errors=%d", hits, failures)
			}
		})
	}
}