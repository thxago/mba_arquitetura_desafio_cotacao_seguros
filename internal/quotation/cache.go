package quotation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type QuoteCache interface {
	Get(context.Context, string) ([]byte, error)
	Set(context.Context, string, []byte, time.Time) error
}

type cacheEntry struct {
	Version   int           `json:"version"`
	TenantID  string        `json:"tenant_id"`
	Quote     partner.Quote `json:"quote"`
	QuotedAt  time.Time     `json:"quoted_at"`
	ExpiresAt time.Time     `json:"expires_at"`
}

func cacheKey(tenant, partnerName string, normalized Request) (string, error) {
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return fmt.Sprintf("prumo:quotes:v1:%s:%s:%x",
		base64.RawURLEncoding.EncodeToString([]byte(tenant)),
		base64.RawURLEncoding.EncodeToString([]byte(partnerName)), digest), nil
}

func effectiveTTL(ttl time.Duration, seconds int64) time.Duration {
	// Clamp before multiplication, including maliciously large provider validity.
	if seconds < 86400 && time.Duration(seconds)*time.Second < ttl {
		return time.Duration(seconds) * time.Second
	}
	return ttl
}

func (e cacheEntry) valid(tenant, partnerName string, ttl time.Duration, now time.Time) bool {
	return e.Version == 1 && e.TenantID == tenant && e.Quote.Partner == partnerName &&
		e.Quote.IsValid() && !e.QuotedAt.IsZero() && !e.QuotedAt.After(now) &&
		e.ExpiresAt.After(e.QuotedAt) &&
		!e.ExpiresAt.After(e.QuotedAt.Add(effectiveTTL(ttl, e.Quote.ValidForSeconds))) &&
		e.ExpiresAt.Sub(now) >= time.Second
}

func (e cacheEntry) presentation(source string) Quote {
	return Quote{Quote: e.Quote, Origin: &Origin{
		Source: source, QuotedAt: e.QuotedAt, ExpiresAt: e.ExpiresAt,
	}}
}

func (s *Service) quoteWithCache(ctx context.Context, tenant string, p platform.Partner, request partnerRequest) (Quote, bool, error) {
	key, err := cacheKey(tenant, p.Name, request.Request)
	if err != nil {
		return Quote{}, false, err
	}
	payload, err := s.cache.Get(ctx, key)
	unavailable := err != nil && !errors.Is(err, cache.ErrMiss)
	if ctx.Err() != nil {
		return Quote{}, unavailable, ctx.Err()
	}
	var entry cacheEntry
	if err == nil && json.Unmarshal(payload, &entry) == nil &&
		entry.valid(tenant, p.Name, s.cacheTTL, time.Now().UTC()) {
		return entry.presentation("cache"), unavailable, nil
	}

	requestedAt := time.Now().UTC()
	quote, err := s.quoter.Quote(ctx, p, request)
	if err != nil {
		return Quote{}, unavailable, err
	}
	if !quote.RequestedAt.IsZero() {
		requestedAt = quote.RequestedAt.UTC()
	}
	if !quote.IsValid() || quote.Partner != p.Name || requestedAt.After(time.Now()) {
		return Quote{}, unavailable, &partner.Error{Partner: p.Name, Kind: "invalid_response", Reason: "invalid quote origin"}
	}
	entry = cacheEntry{
		Version: 1, TenantID: tenant, Quote: quote, QuotedAt: requestedAt,
		ExpiresAt: requestedAt.Add(effectiveTTL(s.cacheTTL, quote.ValidForSeconds)),
	}
	if !entry.valid(tenant, p.Name, s.cacheTTL, time.Now().UTC()) {
		return Quote{}, unavailable, &partner.Error{Partner: p.Name, Kind: "expired", Reason: "quote expired before caching"}
	}
	payload, err = json.Marshal(entry)
	if err != nil {
		return Quote{}, unavailable, err
	}
	if err := s.cache.Set(ctx, key, payload, entry.ExpiresAt); err != nil {
		unavailable = true
	}
	if ctx.Err() != nil {
		return Quote{}, unavailable, ctx.Err()
	}
	return entry.presentation("partner"), unavailable, nil
}