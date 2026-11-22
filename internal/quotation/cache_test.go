package quotation

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type memoryQuoteCache struct {
	mu             sync.Mutex
	values         map[string][]byte
	expirations    map[string]time.Time
	getErr, setErr error
	gets, sets     int
}

func newMemoryCache() *memoryQuoteCache {
	return &memoryQuoteCache{values: make(map[string][]byte), expirations: make(map[string]time.Time)}
}

func (c *memoryQuoteCache) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.getErr != nil {
		return nil, c.getErr
	}
	value, ok := c.values[key]
	if !ok {
		return nil, cache.ErrMiss
	}
	// Deliberately retain expired bytes to prove application-side expiry checks.
	return value, nil
}

func (c *memoryQuoteCache) Set(_ context.Context, key string, value []byte, expiresAt time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	if c.setErr != nil {
		return c.setErr
	}
	c.values[key], c.expirations[key] = value, expiresAt
	return nil
}

type cacheQuoterFunc func(context.Context, platform.Partner, any) (partner.Quote, error)

func (f cacheQuoterFunc) Quote(ctx context.Context, p platform.Partner, request any) (partner.Quote, error) {
	return f(ctx, p, request)
}

func originalQuote(p platform.Partner, seconds int64) partner.Quote {
	return partner.Quote{
		Partner: p.Name, QuoteID: "original-q", PremiumCents: 12345, CoverageCents: 100000,
		Currency: "BRL", ValidForSeconds: seconds, RequestedAt: time.Now().UTC(),
	}
}

func cachedService(t *testing.T, partners []platform.Partner, quoter Quoter, store QuoteCache, ttl time.Duration) *Service {
	t.Helper()
	service, err := NewCachedService(partners, quoter, store, platform.Cache{
		Address: "127.0.0.1:6379", TTL: ttl, OperationTimeout: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestCacheKeyMatchesCanonicalContractAndIncludesEveryField(t *testing.T) {
	request := validRequest()
	key, err := cacheKey("corretora-a", "partner-slow", request)
	if err != nil {
		t.Fatal(err)
	}
	canonical := `{"driver":{"document":"12345678901","birth_year":1988},"vehicle":{"plate":"ABC1D23","model":"Gol 1.0","year":2020,"value_cents":8500000},"coverage":"comprehensive"}`
	want := fmt.Sprintf("prumo:quotes:v1:Y29ycmV0b3JhLWE:cGFydG5lci1zbG93:%x", sha256.Sum256([]byte(canonical)))
	if key != want {
		t.Fatalf("key %q, want %q", key, want)
	}
	changes := []func(*Request){
		func(r *Request) { r.Driver.Document = "different" },
		func(r *Request) { r.Driver.BirthYear++ },
		func(r *Request) { r.Vehicle.Plate = "XYZ9Z99" },
		func(r *Request) { r.Vehicle.Model = `Sedan "x" & \ special` },
		func(r *Request) { r.Vehicle.Year++ },
		func(r *Request) { r.Vehicle.ValueCents++ },
		func(r *Request) { r.Coverage = "third_party" },
	}
	for i, change := range changes {
		changed := request
		change(&changed)
		other, err := cacheKey("corretora-a", "partner-slow", changed)
		if err != nil || other == key {
			t.Fatalf("field %d did not change key: %s %v", i, other, err)
		}
	}
	a, _ := cacheKey("a:b", "c", request)
	b, _ := cacheKey("a", "b:c", request)
	if a == b {
		t.Fatal("tenant and partner segment boundaries collide")
	}
}

func TestCacheIsolatesTenantsAndNormalizedHitPreservesOriginalExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newMemoryCache()
		calls := 0
		quoter := cacheQuoterFunc(func(_ context.Context, p platform.Partner, raw any) (partner.Quote, error) {
			calls++
			request := raw.(partnerRequest)
			q := originalQuote(p, 300)
			q.QuoteID = request.Broker + "-quote"
			if request.Broker == "corretora-b" {
				q.PremiumCents = 54321
			}
			time.Sleep(1500 * time.Millisecond)
			return q, nil
		})
		service := cachedService(t, threePartners[:1], quoter, store, 300*time.Second)
		first, err := service.Quote(context.Background(), "corretora-a", validRequest())
		if err != nil {
			t.Fatal(err)
		}
		initial := first.Quotes[0]
		if initial.Source != "partner" || initial.ValidForSeconds != 298 || initial.AgeSeconds != 1 {
			t.Fatalf("fresh quote has wrong remaining validity: %+v", initial)
		}
		time.Sleep(2 * time.Second)
		variant := validRequest()
		variant.Driver.Document = " 12345678901 "
		variant.Vehicle.Plate = " abc1d23 "
		variant.Vehicle.Model = " Gol 1.0 "
		variant.Coverage = ""
		hit, err := service.Quote(context.Background(), "corretora-a", variant)
		if err != nil {
			t.Fatal(err)
		}
		reused := hit.Quotes[0]
		if calls != 1 || store.sets != 1 || reused.Source != "cache" ||
			reused.QuoteID != initial.QuoteID || reused.PremiumCents != initial.PremiumCents ||
			!reused.QuotedAt.Equal(initial.QuotedAt) || !reused.ExpiresAt.Equal(initial.ExpiresAt) ||
			reused.ValidForSeconds != 296 || reused.AgeSeconds != 3 {
			t.Fatalf("hit renewed or changed original quote: calls=%d writes=%d quote=%+v", calls, store.sets, reused)
		}
		second, err := service.Quote(context.Background(), "corretora-b", validRequest())
		if err != nil {
			t.Fatal(err)
		}
		if calls != 2 || len(store.values) != 2 || second.Quotes[0].PremiumCents == initial.PremiumCents ||
			second.Quotes[0].QuoteID == initial.QuoteID || second.Quotes[0].Source != "partner" {
			t.Fatal("another tenant received the first tenant's cached quote")
		}
		for key, value := range store.values {
			var envelope cacheEntry
			if err := json.Unmarshal(value, &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Quote.ValidForSeconds != 300 || envelope.ExpiresAt.Sub(envelope.QuotedAt) != 300*time.Second ||
				!store.expirations[key].Equal(envelope.ExpiresAt) || strings.Contains(string(value), `"driver"`) ||
				strings.Contains(string(value), `"vehicle"`) {
				t.Fatalf("stored envelope lost original validity or duplicated risk: %s", value)
			}
		}
	})
}

func TestCacheExpiresAtEffectiveValidityAndNeverServesStale(t *testing.T) {
	for _, seconds := range []int64{10, 300, math.MaxInt64} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newMemoryCache()
				calls := 0
				fail := false
				quoter := cacheQuoterFunc(func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
					calls++
					if fail {
						return partner.Quote{}, &partner.Error{Partner: p.Name, Kind: "circuit_open", Reason: "open"}
					}
					return originalQuote(p, seconds), nil
				})
				service := cachedService(t, threePartners[:1], quoter, store, 30*time.Second)
				first, err := service.Quote(context.Background(), "corretora-a", validRequest())
				if err != nil {
					t.Fatal(err)
				}
				expiry := first.Quotes[0].ExpiresAt
				time.Sleep(expiry.Sub(time.Now()) - time.Second)
				if _, err := service.Quote(context.Background(), "corretora-a", validRequest()); err != nil || calls != 1 {
					t.Fatalf("one full second of validity should still hit: calls=%d err=%v", calls, err)
				}
				time.Sleep(time.Nanosecond)
				if _, err := service.Quote(context.Background(), "corretora-a", validRequest()); err != nil || calls != 2 {
					t.Fatalf("less than one second must miss: calls=%d err=%v", calls, err)
				}
				var current cacheEntry
				for _, payload := range store.values {
					_ = json.Unmarshal(payload, &current)
				}
				time.Sleep(current.ExpiresAt.Sub(time.Now()))
				fail = true
				response, err := service.Quote(context.Background(), "corretora-a", validRequest())
				if err != nil || response.Status != statusUnavailable || response.Error != "no_valid_quotes" ||
					len(response.Quotes) != 0 || len(response.UnavailablePartners) != 1 ||
					response.UnavailablePartners[0].Reason != "circuit_open" || calls != 3 {
					t.Fatalf("expired cache masked open circuit: response=%+v err=%v calls=%d", response, err, calls)
				}
			})
		})
	}
}

func TestCacheRejectsIncoherentEntriesAndInvalidFreshQuotes(t *testing.T) {
	cases := map[string]func(*cacheEntry){
		"version":   func(e *cacheEntry) { e.Version++ },
		"tenant":    func(e *cacheEntry) { e.TenantID = "corretora-b" },
		"partner":   func(e *cacheEntry) { e.Quote.Partner = "partner-flaky" },
		"invalid":   func(e *cacheEntry) { e.Quote.QuoteID = "" },
		"future":    func(e *cacheEntry) { e.QuotedAt = time.Now().Add(time.Hour) },
		"extended":  func(e *cacheEntry) { e.ExpiresAt = e.QuotedAt.Add(301 * time.Second) },
		"backwards": func(e *cacheEntry) { e.ExpiresAt = e.QuotedAt },
		"zero time": func(e *cacheEntry) { e.QuotedAt = time.Time{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newMemoryCache()
				p := threePartners[0]
				entry := cacheEntry{Version: 1, TenantID: "corretora-a", Quote: originalQuote(p, 300),
					QuotedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(300 * time.Second)}
				mutate(&entry)
				key, _ := cacheKey("corretora-a", p.Name, validRequest())
				store.values[key], _ = json.Marshal(entry)
				calls := 0
				service := cachedService(t, threePartners[:1], cacheQuoterFunc(func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
					calls++
					return originalQuote(p, 300), nil
				}), store, 300*time.Second)
				response, err := service.Quote(context.Background(), "corretora-a", validRequest())
				if err != nil || calls != 1 || response.Quotes[0].Source != "partner" || store.sets != 1 {
					t.Fatalf("incoherent entry was not replaced: calls=%d err=%v", calls, err)
				}
			})
		})
	}
	t.Run("broken cache and invalid provider", func(t *testing.T) {
		store := newMemoryCache()
		key, _ := cacheKey("corretora-a", threePartners[0].Name, validRequest())
		store.values[key] = []byte(`{`)
		service := cachedService(t, threePartners[:1], cacheQuoterFunc(func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
			q := originalQuote(p, 300)
			q.PremiumCents = 0
			return q, nil
		}), store, 300*time.Second)
		response, err := service.Quote(context.Background(), "corretora-a", validRequest())
		if err != nil || response.Status != statusUnavailable || len(response.Quotes) != 0 ||
			len(response.UnavailablePartners) != 1 || response.UnavailablePartners[0].Reason != "invalid_response" || store.sets != 0 {
			t.Fatal("invalid provider response was accepted or cached")
		}
	})
}

func TestRedisErrorsDoNotDiscardFreshQuotes(t *testing.T) {
	for _, failure := range []string{"get", "set", "both"} {
		t.Run(failure, func(t *testing.T) {
			store := newMemoryCache()
			if failure != "set" {
				store.getErr = context.DeadlineExceeded
			}
			if failure != "get" {
				store.setErr = errors.New("Redis unavailable")
			}
			service := cachedService(t, threePartners[:1], cacheQuoterFunc(func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
				return originalQuote(p, 300), nil
			}), store, 300*time.Second)
			response, err := service.Quote(context.Background(), "corretora-a", validRequest())
			if err != nil || len(response.Quotes) != 1 || response.Quotes[0].Source != "partner" ||
				response.CacheStatus != "unavailable" || !response.Degraded {
				t.Fatalf("Redis failure discarded fresh quote or was hidden: response=%+v err=%v", response, err)
			}
		})
	}
}

func TestQuoteCannotExpireDuringPartnerCallOrFinalComposition(t *testing.T) {
	for _, duringCall := range []bool{false, true} {
		t.Run(fmt.Sprint(duringCall), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newMemoryCache()
				var calls atomic.Int32
				service := cachedService(t, threePartners[:2], cacheQuoterFunc(func(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
					calls.Add(1)
					quote := originalQuote(p, 300)
					first := p.Name == threePartners[0].Name
					if first {
						quote.ValidForSeconds = 2
					}
					if (duringCall && first) || (!duringCall && !first) {
						time.Sleep(1500 * time.Millisecond)
					}
					return quote, nil
				}), store, 300*time.Second)
				response, err := service.Quote(context.Background(), "corretora-a", validRequest())
				if err != nil || response.Status != statusPartial || !response.Degraded ||
					len(response.Quotes) != 1 || response.Quotes[0].Partner != threePartners[1].Name ||
					response.Quotes[0].ValidForSeconds < 1 || len(response.UnavailablePartners) != 1 ||
					response.UnavailablePartners[0] != (UnavailablePartner{Partner: threePartners[0].Name, Reason: "expired"}) {
					t.Fatalf("expired quote was presented: response=%+v err=%v", response, err)
				}
				if calls.Load() != 2 {
					t.Fatalf("expired proposal was queried again: calls=%d want 2", calls.Load())
				}
			})
		})
	}
}