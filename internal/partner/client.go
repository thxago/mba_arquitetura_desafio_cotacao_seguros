package partner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/sony/gobreaker/v2"
)

type Quote struct {
	Partner         string `json:"partner"`
	QuoteID         string `json:"quote_id"`
	PremiumCents    int64  `json:"premium_cents"`
	Currency        string `json:"currency"`
	CoverageCents   int64  `json:"coverage_cents"`
	ValidForSeconds int64  `json:"valid_for_seconds"`
}

type Client struct {
	http     *http.Client
	partners map[string]*protectedPartner
}

type protectedPartner struct {
	endpoint string
	http     *http.Client
	breaker  *gobreaker.CircuitBreaker[Quote]
}

func NewClient() *Client {
	return &Client{http: newHTTPClient(0)}
}

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: platform.InstrumentTransport(http.DefaultTransport),
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func NewResilientClient(partners []platform.Partner, cfg platform.Resilience) (*Client, error) {
	if err := cfg.Validate(partners); err != nil {
		return nil, err
	}
	client := &Client{partners: make(map[string]*protectedPartner, len(partners))}
	for _, p := range partners {
		client.partners[p.Name] = &protectedPartner{
			endpoint: p.BaseURL,
			http:     newHTTPClient(cfg.PartnerTimeouts[p.Name]),
			breaker: gobreaker.NewCircuitBreaker[Quote](gobreaker.Settings{
				Name:        p.Name,
				MaxRequests: cfg.BreakerMaxRequests,
				Interval:    cfg.BreakerInterval,
				Timeout:     cfg.BreakerOpenTimeout,
				ReadyToTrip: func(counts gobreaker.Counts) bool {
					return counts.ConsecutiveFailures >= cfg.BreakerFailureThreshold
				},
				IsExcluded: func(err error) bool {
					var canceled *consumerCancellation
					return errors.As(err, &canceled)
				},
				OnStateChange: func(name string, from, to gobreaker.State) {
					log.Printf("partner breaker: partner=%s from=%s to=%s", name, from, to)
				},
			}),
		}
	}
	return client, nil
}

const responseLimit = 1 << 20

func (c *Client) Quote(ctx context.Context, p platform.Partner, request any) (Quote, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Kind: "invalid_request", Reason: fmt.Sprintf("invalid request: %v", err), Cause: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/quotes", bytes.NewReader(body))
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Kind: "invalid_request", Reason: err.Error(), Cause: err}
	}
	req.Header.Set("Content-Type", "application/json")

	if c.partners == nil {
		return executeQuote(c.http, req, p.Name)
	}
	protected, ok := c.partners[p.Name]
	if !ok || protected.endpoint != p.BaseURL {
		return Quote{}, &Error{Partner: p.Name, Kind: "invalid_request", Reason: "partner is not configured"}
	}
	if err := ctx.Err(); err != nil {
		return Quote{}, requestError(ctx, p.Name, err)
	}
	quote, err := protected.breaker.Execute(func() (Quote, error) {
		return executeQuote(protected.http, req, p.Name)
	})
	switch {
	case errors.Is(err, gobreaker.ErrOpenState):
		return Quote{}, &Error{Partner: p.Name, Kind: "circuit_open", Reason: "circuit is open", Cause: err}
	case errors.Is(err, gobreaker.ErrTooManyRequests):
		return Quote{}, &Error{Partner: p.Name, Kind: "half_open_limit", Reason: "half-open probe is in progress", Cause: err}
	default:
		return quote, err
	}
}

func executeQuote(client *http.Client, req *http.Request, name string) (Quote, error) {
	response, err := client.Do(req)
	if err != nil {
		return Quote{}, requestError(req.Context(), name, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
		if err := req.Context().Err(); err != nil {
			return Quote{}, requestError(req.Context(), name, err)
		}
		return Quote{}, &Error{
			Partner: name,
			Status:  response.StatusCode,
			Kind:    "upstream_error",
			Reason:  fmt.Sprintf("partner replied %d", response.StatusCode),
		}
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return Quote{}, requestError(req.Context(), name, err)
	}
	if err := req.Context().Err(); err != nil {
		return Quote{}, requestError(req.Context(), name, err)
	}
	var quote Quote
	if err := json.Unmarshal(body, &quote); err != nil || len(body) > responseLimit {
		return Quote{}, &Error{Partner: name, Kind: "invalid_response", Reason: "unreadable or oversized response", Cause: err}
	}
	if strings.TrimSpace(quote.QuoteID) == "" || quote.PremiumCents <= 0 ||
		quote.CoverageCents <= 0 || quote.Currency != "BRL" || quote.ValidForSeconds <= 0 {
		return Quote{}, &Error{Partner: name, Kind: "invalid_response", Reason: "invalid quote"}
	}

	quote.Partner = name
	return quote, nil
}

type consumerCancellation struct{ cause error }

func (e *consumerCancellation) Error() string { return e.cause.Error() }
func (e *consumerCancellation) Unwrap() error { return e.cause }

func requestError(ctx context.Context, name string, err error) *Error {
	kind := "upstream_error"
	if cause := ctx.Err(); cause != nil {
		kind, err = "consumer_canceled", &consumerCancellation{cause: cause}
	} else {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			kind = "timeout"
		}
	}
	return &Error{Partner: name, Kind: kind, Reason: err.Error(), Cause: err}
}

type Error struct {
	Partner string
	Status  int
	Reason  string
	Kind    string
	Cause   error
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Error() string {
	return fmt.Sprintf("partner %s: %s", e.Partner, e.Reason)
}