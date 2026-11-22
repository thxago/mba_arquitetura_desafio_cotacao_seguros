package quotation

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type Quoter interface {
	Quote(ctx context.Context, p platform.Partner, request any) (partner.Quote, error)
}

type Service struct {
	partners []platform.Partner
	quoter   Quoter
	cache    QuoteCache
	cacheTTL time.Duration
}

func NewService(partners []platform.Partner, quoter Quoter) *Service {
	return &Service{partners: partners, quoter: quoter}
}

func NewCachedService(partners []platform.Partner, quoter Quoter, cache QuoteCache, cfg platform.Cache) (*Service, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cache == nil {
		return nil, fmt.Errorf("quote cache is required")
	}
	service := NewService(partners, quoter)
	service.cache, service.cacheTTL = cache, cfg.TTL
	return service, nil
}

func (s *Service) Quote(ctx context.Context, tenant string, request Request) (Response, error) {
	start := time.Now()
	if s.cache != nil {
		if err := request.Normalize(); err != nil {
			return Response{}, err
		}
	}
	forPartner := partnerRequest{Broker: tenant, Request: request}

	quotes := make([]Quote, 0, len(s.partners))
	cacheUnavailable := false
	for _, p := range s.partners {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		var quote Quote
		var err error
		if s.cache == nil {
			quote.Quote, err = s.quoter.Quote(ctx, p, forPartner)
		} else {
			var unavailable bool
			quote, unavailable, err = s.quoteWithCache(ctx, tenant, p, forPartner)
			cacheUnavailable = cacheUnavailable || unavailable
		}
		if err != nil {
			return Response{}, err
		}
		quotes = append(quotes, quote)
	}

	now := time.Now().UTC()
	for i := range quotes {
		if quotes[i].Origin == nil {
			continue
		}
		remaining := int64(quotes[i].ExpiresAt.Sub(now) / time.Second)
		if remaining < 1 {
			return Response{}, &partner.Error{Partner: quotes[i].Partner, Kind: "expired", Reason: "quote expired before presentation"}
		}
		quotes[i].ValidForSeconds = remaining
		quotes[i].AgeSeconds = int64(now.Sub(quotes[i].QuotedAt) / time.Second)
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	sort.Slice(quotes, func(i, j int) bool {
		return quotes[i].PremiumCents < quotes[j].PremiumCents
	})

	response := Response{
		TenantID:  tenant,
		Quotes:    quotes,
		ElapsedMs: time.Since(start).Milliseconds(),
		Degraded:  cacheUnavailable,
	}
	if s.cache != nil {
		response.CacheStatus = "available"
		if cacheUnavailable {
			response.CacheStatus = "unavailable"
		}
	}
	return response, nil
}