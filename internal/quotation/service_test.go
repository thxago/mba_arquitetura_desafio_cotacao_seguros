package quotation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

var threePartners = []platform.Partner{
	{Name: "partner-slow", BaseURL: "http://slow"},
	{Name: "partner-flaky", BaseURL: "http://flaky"},
	{Name: "partner-degrading", BaseURL: "http://degrading"},
}

func validRequest() Request {
	r := Request{
		Driver:  Driver{Document: "12345678901", BirthYear: 1988},
		Vehicle: Vehicle{Plate: "ABC1D23", Model: "Gol 1.0", Year: 2020, ValueCents: 8500000},
	}
	if err := r.Normalize(); err != nil {
		panic(err)
	}
	return r
}

type fakeQuoter struct {
	delay      time.Duration
	premiums   map[string]int64
	failOn     string
	calls      []string
	requests   []any
	concurrent atomic.Int32
	peak       atomic.Int32
}

func (f *fakeQuoter) Quote(_ context.Context, p platform.Partner, request any) (partner.Quote, error) {
	if now := f.concurrent.Add(1); now > f.peak.Load() {
		f.peak.Store(now)
	}
	defer f.concurrent.Add(-1)

	f.calls = append(f.calls, p.Name)
	f.requests = append(f.requests, request)
	time.Sleep(f.delay)

	if p.Name == f.failOn {
		return partner.Quote{}, &partner.Error{Partner: p.Name, Status: 503, Reason: "partner answered 503"}
	}
	return partner.Quote{Partner: p.Name, PremiumCents: f.premiums[p.Name], Currency: "BRL"}, nil
}

func defaultPremiums() map[string]int64 {
	return map[string]int64{"partner-slow": 180000, "partner-flaky": 90000, "partner-degrading": 120000}
}

func TestQuoteAggregatesTheThreePartnersSortedByPremium(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums()}
	response, err := NewService(threePartners, quoter).Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if len(response.Quotes) != 3 {
		t.Fatalf("%d quotes, expected 3", len(response.Quotes))
	}
	expected := []string{"partner-flaky", "partner-degrading", "partner-slow"}
	for i, name := range expected {
		if response.Quotes[i].Partner != name {
			t.Errorf("quote %d is from %q, expected from %q", i, response.Quotes[i].Partner, name)
		}
	}
	if response.TenantID != "corretora-a" {
		t.Errorf("tenant_id %q, expected corretora-a", response.TenantID)
	}
}

func TestQuoteCallsThePartnersSerially(t *testing.T) {
	quoter := &fakeQuoter{delay: 40 * time.Millisecond, premiums: defaultPremiums()}

	start := time.Now()
	if _, err := NewService(threePartners, quoter).Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("Quote: %v", err)
	}
	elapsed := time.Since(start)

	if minimum := 3 * quoter.delay; elapsed < minimum {
		t.Fatalf("aggregation took %s; serially it should take at least %s", elapsed, minimum)
	}
	if peak := quoter.peak.Load(); peak != 1 {
		t.Fatalf("%d concurrent calls at the peak, expected 1 (serial calls)", peak)
	}
}

func TestOnePartnerDownBringsDownTheWholeRequest(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums(), failOn: "partner-flaky"}

	_, err := NewService(threePartners, quoter).Quote(context.Background(), "corretora-a", validRequest())
	if err == nil {
		t.Fatal("one partner down and the request answered success — there is a fallback where there should be none")
	}

	var failure *partner.Error
	if !errors.As(err, &failure) || failure.Partner != "partner-flaky" {
		t.Fatalf("error %v does not identify the partner that failed", err)
	}

	if last := quoter.calls[len(quoter.calls)-1]; last != "partner-flaky" {
		t.Errorf("last partner called was %q, expected partner-flaky", last)
	}
}

func TestBrokerGoesInThePartnerRequest(t *testing.T) {
	quoter := &fakeQuoter{premiums: defaultPremiums()}
	service := NewService(threePartners[:1], quoter)

	if _, err := service.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if _, err := service.Quote(context.Background(), "corretora-b", validRequest()); err != nil {
		t.Fatalf("Quote: %v", err)
	}

	toA, toB := serialize(t, quoter.requests[0]), serialize(t, quoter.requests[1])
	if toA == toB {
		t.Fatalf("different brokers produced the same request to the partner: %s", toA)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(toA), &body); err != nil {
		t.Fatalf("request to the partner is not JSON: %v", err)
	}
	if body["broker"] != "corretora-a" {
		t.Errorf("broker %v, expected corretora-a", body["broker"])
	}
	if _, ok := body["vehicle"]; !ok {
		t.Errorf("request to the partner does not carry the vehicle: %s", toA)
	}
}

func serialize(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func TestPartialQuoteContinuesAfterFailuresAndPreservesSortedOrigins(t *testing.T) {
	for _, reason := range []string{"timeout", "circuit_open", "half_open_limit", "upstream_error", "invalid_response", "expired", "private_reason"} {
		t.Run(reason, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := make(chan string, len(threePartners))
				service := cachedService(t, threePartners, cacheQuoterFunc(func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
					calls <- p.Name
					if p.Name == threePartners[0].Name {
						return partner.Quote{}, fmt.Errorf("wrapped: %w", &partner.Error{
							Partner: "untrusted-name", Kind: reason, Reason: "http://internal/private-details",
							Cause: context.DeadlineExceeded,
						})
					}
					quote := originalQuote(p, 300)
					quote.QuoteID = "original-" + p.Name
					quote.PremiumCents = defaultPremiums()[p.Name]
					return quote, nil
				}), newMemoryCache(), 300*time.Second)
				response, err := service.Quote(context.Background(), "corretora-a", validRequest())
				wantReason := reason
				if reason == "private_reason" {
					wantReason = "upstream_error"
				}
				if err != nil || response.Status != statusPartial || !response.Degraded ||
					response.CacheStatus != "available" || len(response.Quotes) != 2 ||
					response.Error != "" || response.Action != "" ||
					!reflect.DeepEqual(response.UnavailablePartners, []UnavailablePartner{{Partner: threePartners[0].Name, Reason: wantReason}}) {
					t.Fatalf("unexpected partial response: %+v err=%v", response, err)
				}
				wantCalls := []string{threePartners[0].Name, threePartners[1].Name, threePartners[2].Name}
				close(calls)
				var gotCalls []string
				for name := range calls {
					gotCalls = append(gotCalls, name)
				}
				slices.Sort(gotCalls)
				slices.Sort(wantCalls)
				if !reflect.DeepEqual(gotCalls, wantCalls) {
					t.Fatalf("failed partner stopped or retried aggregation: %v", gotCalls)
				}
				for i, name := range []string{"partner-flaky", "partner-degrading"} {
					quote := response.Quotes[i]
					if quote.Partner != name || quote.QuoteID != "original-"+name || quote.PremiumCents != defaultPremiums()[name] ||
						quote.Source != "partner" || quote.ValidForSeconds != 300 || quote.AgeSeconds != 0 ||
						quote.ExpiresAt.Sub(quote.QuotedAt) != 300*time.Second {
						t.Fatalf("sort detached original proposal or metadata: %+v", quote)
					}
				}
				body := serialize(t, response)
				if strings.Contains(body, "http://internal") || strings.Contains(body, "untrusted-name") || strings.Contains(body, "private_reason") {
					t.Fatalf("internal error leaked to contract: %s", body)
				}
			})
		})
	}
}

func TestConsumerCancellationDiscardsPartialResultsAndStopsPurchasing(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				if deadline {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
				}
				defer cancel()
				var calls atomic.Int32
				started := make(chan struct{}, len(threePartners))
				store := newMemoryCache()
				service := cachedService(t, threePartners, cacheQuoterFunc(func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
					calls.Add(1)
					started <- struct{}{}
					<-ctx.Done()
					return partner.Quote{}, ctx.Err()
				}), store, 300*time.Second)
				if !deadline {
					go func() {
						for range threePartners {
							<-started
						}
						cancel()
					}()
				}
				response, err := service.Quote(ctx, "corretora-a", validRequest())
				if !errors.Is(err, ctx.Err()) || err == nil || len(response.Quotes) != 0 ||
					calls.Load() != 3 || store.gets != 3 || store.sets != 0 {
					t.Fatalf("canceled consumer got partial output or bought more work: response=%+v calls=%d writes=%d err=%v",
						response, calls.Load(), store.sets, err)
				}

			})
		})
	}
}

func TestResilientAggregationOverlapsPartnersAndPreservesOrigins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		delays := map[string]time.Duration{
			"partner-slow": 3 * time.Second, "partner-flaky": time.Second, "partner-degrading": 2 * time.Second,
		}
		service := cachedService(t, threePartners, cacheQuoterFunc(func(_ context.Context, p platform.Partner, raw any) (partner.Quote, error) {
			if raw.(partnerRequest).Broker != "corretora-a" {
				t.Error("worker lost tenant")
			}
			q := originalQuote(p, 300)
			q.QuoteID = p.Name
			q.PremiumCents = defaultPremiums()[p.Name]
			time.Sleep(delays[p.Name])
			return q, nil
		}), newMemoryCache(), 300*time.Second)
		start := time.Now()
		response, err := service.Quote(context.Background(), "corretora-a", validRequest())
		if err != nil || response.Status != statusComplete || len(response.Quotes) != 3 ||
			time.Since(start) != 3*time.Second {
			t.Fatalf("partners did not overlap: response=%+v elapsed=%s err=%v", response, time.Since(start), err)
		}
		for i, name := range []string{"partner-flaky", "partner-degrading", "partner-slow"} {
			q := response.Quotes[i]
			if q.Partner != name || q.QuoteID != name || q.PremiumCents != defaultPremiums()[name] ||
				q.Source != "partner" || q.ValidForSeconds != 297 || q.AgeSeconds != 3 {
				t.Fatalf("completion order detached origin or validity: %+v", q)
			}
		}
	})
}

func TestCanceledConsumerDoesNotStartParallelWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := newMemoryCache()
	service := cachedService(t, threePartners, cacheQuoterFunc(func(context.Context, platform.Partner, any) (partner.Quote, error) {
		t.Error("started HTTP for canceled consumer")
		return partner.Quote{}, nil
	}), store, 300*time.Second)
	response, err := service.Quote(ctx, "corretora-a", validRequest())
	if !errors.Is(err, context.Canceled) || len(response.Quotes) != 0 || store.gets != 0 || store.sets != 0 {
		t.Fatalf("canceled request started work: %+v %v", response, err)
	}
}