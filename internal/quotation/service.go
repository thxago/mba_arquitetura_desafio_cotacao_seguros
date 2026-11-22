package quotation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type Quoter interface {
	Quote(ctx context.Context, p platform.Partner, request any) (partner.Quote, error)
}

type Service struct {
	partners     []platform.Partner
	quoter       Quoter
	cache        QuoteCache
	cacheTTL     time.Duration
	cacheLookups metric.Int64Counter
}

// NewService retains the baseline contract; the API uses NewCachedService.
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
	lookups, err := otel.Meter("prumo/quotation").Int64Counter("prumo_cache_lookups",
		metric.WithDescription("Quote cache lookups by partner and result"))
	if err != nil {
		return nil, fmt.Errorf("cache lookups metric: %w", err)
	}
	service := NewService(partners, quoter)
	service.cache, service.cacheTTL = cache, cfg.TTL
	service.cacheLookups = lookups
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
	failures := make(map[string]string, len(s.partners))
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
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if err != nil {
			if s.cache == nil {
				return Response{}, err
			}
			failures[p.Name] = unavailableReason(err)
			continue
		}
		quotes = append(quotes, quote)
	}

	now := time.Now().UTC()
	valid := quotes[:0]
	for _, quote := range quotes {
		if quote.Origin != nil {
			if quote.QuotedAt.After(now) {
				failures[quote.Partner] = "invalid_response"
				continue
			}
			remaining := int64(quote.ExpiresAt.Sub(now) / time.Second)
			if remaining < 1 {
				failures[quote.Partner] = "expired"
				continue
			}
			quote.ValidForSeconds = remaining
			quote.AgeSeconds = int64(now.Sub(quote.QuotedAt) / time.Second)
		}
		valid = append(valid, quote)
	}
	quotes = valid
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	for _, quote := range quotes {
		if quote.Origin != nil && quote.Source == "cache" {
			trace.SpanFromContext(ctx).AddEvent("quotation.cache_hit",
				trace.WithAttributes(attribute.String("partner", quote.Partner)))
		}
	}
	sort.Slice(quotes, func(i, j int) bool {
		return quotes[i].PremiumCents < quotes[j].PremiumCents
	})

	response := Response{
		TenantID:            tenant,
		Quotes:              quotes,
		ElapsedMs:           time.Since(start).Milliseconds(),
		Status:              statusComplete,
		Degraded:            cacheUnavailable,
		UnavailablePartners: make([]UnavailablePartner, 0, len(failures)),
	}
	for _, p := range s.partners {
		if reason, missing := failures[p.Name]; missing {
			response.UnavailablePartners = append(response.UnavailablePartners, UnavailablePartner{Partner: p.Name, Reason: reason})
		}
	}
	if len(failures) > 0 {
		response.Status, response.Degraded = statusPartial, true
	}
	if s.cache != nil && len(quotes) == 0 {
		response.Status, response.Degraded = statusUnavailable, true
		response.Error = "no_valid_quotes"
		response.Action = "Solicite uma nova cotação mais tarde ou contate o suporte da corretora."
	}
	if s.cache != nil {
		response.CacheStatus = "available"
		if cacheUnavailable {
			response.CacheStatus = "unavailable"
		}
	}
	return response, nil
}

func unavailableReason(err error) string {
	var failure *partner.Error
	if errors.As(err, &failure) {
		switch failure.Kind {
		case "timeout", "circuit_open", "half_open_limit", "upstream_error", "invalid_response", "expired":
			return failure.Kind
		}
	}
	return "upstream_error"
}