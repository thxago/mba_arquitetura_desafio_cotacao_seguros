package partner

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestBreakerTelemetryCountsRecoveryAndMarksBothRejections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		calls := 0
		probing := false
		entered, release := make(chan struct{}), make(chan struct{})
		client, p := resilientClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if probing {
				close(entered)
				<-release
				return reply(http.StatusOK, validQuote), nil
			}
			return reply(http.StatusServiceUnavailable, ""), nil
		}))
		call := func() error {
			ctx, span := traces.Tracer("test").Start(context.Background(), "quotation")
			defer span.End()
			_, err := client.Quote(ctx, p, nil)
			return err
		}
		for i := 0; i < 3; i++ {
			requireKind(t, call(), "upstream_error")
		}
		requireKind(t, call(), "circuit_open")
		time.Sleep(5*time.Second + time.Nanosecond)
		probing = true
		done := make(chan error, 1)
		go func() { done <- call() }()
		<-entered
		requireKind(t, call(), "half_open_limit")
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if calls != 4 {
			t.Fatalf("rejected requests reached partner: %d", calls)
		}
		var data metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &data); err != nil {
			t.Fatal(err)
		}
		got := make(map[string]int64)
		for _, scope := range data.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name != "prumo_breaker_transitions" {
					continue
				}
				for _, point := range m.Data.(metricdata.Sum[int64]).DataPoints {
					from, _ := point.Attributes.Value("from")
					to, _ := point.Attributes.Value("to")
					name, _ := point.Attributes.Value("partner")
					if point.Attributes.Len() != 3 || name.AsString() != p.Name {
						t.Fatalf("unexpected transition attributes: %v", point.Attributes)
					}
					got[from.AsString()+"->"+to.AsString()] += point.Value
				}
			}
		}
		if len(got) != 3 || got["closed->open"] != 1 || got["open->half_open"] != 1 || got["half_open->closed"] != 1 {
			t.Fatalf("lost or duplicated transitions: %v", got)
		}
		reasons := make(map[string]int)
		for _, span := range spans.Ended() {
			for _, event := range span.Events() {
				if event.Name != "partner.short_circuit" {
					continue
				}
				if len(event.Attributes) != 2 || event.Attributes[0].Value.AsString() != p.Name {
					t.Fatalf("unexpected rejection event: %+v", event)
				}
				reasons[event.Attributes[1].Value.AsString()]++
			}
		}
		if len(reasons) != 2 || reasons["circuit_open"] != 1 || reasons["half_open_limit"] != 1 {
			t.Fatalf("wrong short-circuit events: %v", reasons)
		}
	})
}