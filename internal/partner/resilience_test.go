package partner

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/sony/gobreaker/v2"
)

const validQuote = `{"quote_id":"q-1","premium_cents":123456,"currency":"BRL","coverage_cents":5000000,"valid_for_seconds":300}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func resilientClient(t *testing.T, transport http.RoundTripper) (*Client, platform.Partner) {
	t.Helper()
	cfg, err := platform.LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewResilientClient(cfg.Partners, cfg.Resilience)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range client.partners {
		p.http.Transport = transport
	}
	return client, cfg.Partners[0]
}

func requireKind(t *testing.T, err error, kind string) {
	t.Helper()
	var failure *Error
	if !errors.As(err, &failure) || failure.Kind != kind {
		t.Fatalf("error %v, want kind %q", err, kind)
	}
}

func requireState(t *testing.T, client *Client, p platform.Partner, want gobreaker.State) {
	t.Helper()
	if state := client.partners[p.Name].breaker.State(); state != want {
		t.Fatalf("state %s, want %s", state, want)
	}
}

func TestBreakerOpensAndRecoversWithOneHalfOpenProbe(t *testing.T) {
	for _, probeSucceeds := range []bool{false, true} {
		t.Run(map[bool]string{false: "reopens", true: "closes"}[probeSucceeds], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				probing := false
				entered, release := make(chan struct{}), make(chan struct{})
				client, p := resilientClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					if probing {
						close(entered)
						<-release
						if probeSucceeds {
							return reply(http.StatusOK, validQuote), nil
						}
					}
					return reply(http.StatusServiceUnavailable, ""), nil
				}))
				requireState(t, client, p, gobreaker.StateClosed)
				for i := 0; i < 3; i++ {
					_, err := client.Quote(context.Background(), p, map[string]string{"broker": "corretora-a"})
					requireKind(t, err, "upstream_error")
				}
				requireState(t, client, p, gobreaker.StateOpen)
				_, err := client.Quote(context.Background(), p, map[string]string{"broker": "corretora-b"})
				requireKind(t, err, "circuit_open")
				if calls != 3 || !errors.Is(err, gobreaker.ErrOpenState) {
					t.Fatalf("open circuit sent HTTP or lost cause: calls=%d err=%v", calls, err)
				}
				time.Sleep(5*time.Second - time.Nanosecond)
				_, err = client.Quote(context.Background(), p, nil)
				requireKind(t, err, "circuit_open")
				time.Sleep(2 * time.Nanosecond)
				requireState(t, client, p, gobreaker.StateHalfOpen)
				probing = true
				done := make(chan error, 1)
				go func() {
					_, err := client.Quote(context.Background(), p, nil)
					done <- err
				}()
				<-entered
				_, err = client.Quote(context.Background(), p, nil)
				requireKind(t, err, "half_open_limit")
				if calls != 4 || !errors.Is(err, gobreaker.ErrTooManyRequests) {
					t.Fatalf("extra half-open request sent HTTP: calls=%d err=%v", calls, err)
				}
				close(release)
				err = <-done
				if probeSucceeds {
					if err != nil {
						t.Fatal(err)
					}
					requireState(t, client, p, gobreaker.StateClosed)
				} else {
					requireKind(t, err, "upstream_error")
					requireState(t, client, p, gobreaker.StateOpen)
					_, err = client.Quote(context.Background(), p, nil)
					requireKind(t, err, "circuit_open")
					time.Sleep(5*time.Second + time.Nanosecond)
					requireState(t, client, p, gobreaker.StateHalfOpen)
				}
			})
		})
	}
}

func TestBreakerCountsOwnTimeoutIncludingResponseBody(t *testing.T) {
	for _, bodyTimeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "headers", true: "body"}[bodyTimeout], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client, _ := resilientClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					if bodyTimeout {
						return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: &waitingBody{ctx: r.Context()}}, nil
					}
					<-r.Context().Done()
					return nil, r.Context().Err()
				}))
				p := platform.Partner{Name: "partner-degrading", BaseURL: "http://localhost:9003"}
				for i := 0; i < 3; i++ {
					start := time.Now()
					_, err := client.Quote(context.Background(), p, nil)
					requireKind(t, err, "timeout")
					if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != time.Second {
						t.Fatalf("own timeout not enforced: elapsed=%v err=%v", time.Since(start), err)
					}
				}
				requireState(t, client, p, gobreaker.StateOpen)
				_, err := client.Quote(context.Background(), p, nil)
				requireKind(t, err, "circuit_open")
				if calls != 3 {
					t.Fatalf("timed-out partner called %d times, want 3", calls)
				}
			})
		})
	}
}

type waitingBody struct{ ctx context.Context }

func (b *waitingBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (*waitingBody) Close() error { return nil }

func TestConsumerCancellationIsExcludedAndReleasesHalfOpenProbe(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "deadline"}[deadline], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				block := false
				succeed := false
				entered := make(chan struct{}, 1)
				client, p := resilientClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if block {
						entered <- struct{}{}
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					if succeed {
						return reply(http.StatusOK, validQuote), nil
					}
					return reply(http.StatusServiceUnavailable, ""), nil
				}))
				cancelCall := func() {
					t.Helper()
					block = true
					var ctx context.Context
					var cancel context.CancelFunc
					if deadline {
						ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
					} else {
						ctx, cancel = context.WithCancel(context.Background())
					}
					defer cancel()
					done := make(chan error, 1)
					go func() {
						_, err := client.Quote(ctx, p, nil)
						done <- err
					}()
					<-entered
					if !deadline {
						cancel()
					}
					err := <-done
					requireKind(t, err, "consumer_canceled")
					if !errors.Is(err, ctx.Err()) {
						t.Fatalf("lost parent context cause: %v", err)
					}
					block = false
				}
				for i := 0; i < 2; i++ {
					_, _ = client.Quote(context.Background(), p, nil)
				}
				cancelCall()
				counts := client.partners[p.Name].breaker.Counts()
				if counts.TotalFailures != 2 || counts.TotalSuccesses != 0 || counts.ConsecutiveFailures != 2 {
					t.Fatalf("cancellation changed health: %+v", counts)
				}
				_, _ = client.Quote(context.Background(), p, nil)
				requireState(t, client, p, gobreaker.StateOpen)
				time.Sleep(5*time.Second + time.Nanosecond)
				cancelCall()
				requireState(t, client, p, gobreaker.StateHalfOpen)
				succeed = true
				if _, err := client.Quote(context.Background(), p, nil); err != nil {
					t.Fatalf("excluded half-open request did not release admission: %v", err)
				}
				requireState(t, client, p, gobreaker.StateClosed)
			})
		})
	}
}

func TestBreakerResetsConsecutiveFailuresOnSuccessAndFixedWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		succeed := false
		client, p := resilientClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			if succeed {
				return reply(http.StatusOK, validQuote), nil
			}
			return reply(http.StatusServiceUnavailable, ""), nil
		}))
		for i := 0; i < 2; i++ {
			_, _ = client.Quote(context.Background(), p, nil)
		}
		succeed = true
		if _, err := client.Quote(context.Background(), p, nil); err != nil {
			t.Fatal(err)
		}
		succeed = false
		for i := 0; i < 2; i++ {
			_, _ = client.Quote(context.Background(), p, nil)
		}
		requireState(t, client, p, gobreaker.StateClosed)
		time.Sleep(10*time.Second + time.Nanosecond)
		_, _ = client.Quote(context.Background(), p, nil)
		requireState(t, client, p, gobreaker.StateClosed)
		if counts := client.partners[p.Name].breaker.Counts(); counts.ConsecutiveFailures != 1 {
			t.Fatalf("fixed window did not reset counts: %+v", counts)
		}
	})
}

func TestPartnerIsolationAndLocalErrorsDoNotCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		client, p := resilientClient(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Port() == "9001" {
				return reply(http.StatusServiceUnavailable, ""), nil
			}
			return reply(http.StatusOK, validQuote), nil
		}))
		for i := 0; i < 3; i++ {
			_, err := client.Quote(context.Background(), p, make(chan int))
			requireKind(t, err, "invalid_request")
			_, err = client.Quote(context.Background(), platform.Partner{Name: p.Name, BaseURL: "http://other"}, nil)
			requireKind(t, err, "invalid_request")
		}
		if calls != 0 || client.partners[p.Name].breaker.Counts().Requests != 0 {
			t.Fatal("local errors reached breaker or HTTP")
		}
		for i := 0; i < 3; i++ {
			_, _ = client.Quote(context.Background(), p, nil)
		}
		other := platform.Partner{Name: "partner-flaky", BaseURL: "http://localhost:9002"}
		if _, err := client.Quote(context.Background(), other, nil); err != nil {
			t.Fatalf("one partner opened another partner's circuit: %v", err)
		}
		requireState(t, client, p, gobreaker.StateOpen)
		requireState(t, client, other, gobreaker.StateClosed)
	})
}

func TestInFlightSuccessDoesNotCloseNewlyOpenedCircuit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		entered, release := make(chan struct{}), make(chan struct{})
		client, p := resilientClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				close(entered)
				<-release
				return reply(http.StatusOK, validQuote), nil
			}
			return reply(http.StatusServiceUnavailable, ""), nil
		}))
		done := make(chan error, 1)
		go func() {
			_, err := client.Quote(context.Background(), p, nil)
			done <- err
		}()
		<-entered
		for i := 0; i < 3; i++ {
			_, err := client.Quote(context.Background(), p, nil)
			requireKind(t, err, "upstream_error")
		}
		requireState(t, client, p, gobreaker.StateOpen)
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("already-admitted request should finish: %v", err)
		}
		requireState(t, client, p, gobreaker.StateOpen)
		_, err := client.Quote(context.Background(), p, nil)
		requireKind(t, err, "circuit_open")
		if calls != 4 {
			t.Fatalf("late success bypassed open circuit: calls=%d", calls)
		}
	})
}

func TestInvalidQuotesAndTransportErrorsOpenBreaker(t *testing.T) {
	cases := map[string]string{
		"empty":      `{}`,
		"broken":     `{`,
		"missing id": strings.Replace(validQuote, `"q-1"`, `""`, 1),
		"zero price": strings.Replace(validQuote, `123456`, `0`, 1),
		"currency":   strings.Replace(validQuote, `"BRL"`, `"USD"`, 1),
		"coverage":   strings.Replace(validQuote, `5000000`, `-1`, 1),
		"validity":   strings.Replace(validQuote, `300`, `0`, 1),
		"trailing":   validQuote + `{}`,
		"oversized":  validQuote + strings.Repeat(" ", responseLimit),
		"transport":  "",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				client, p := resilientClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					if name == "transport" {
						return nil, errors.New("connection failed")
					}
					return reply(http.StatusOK, body), nil
				}))
				for i := 0; i < 3; i++ {
					_, err := client.Quote(context.Background(), p, nil)
					kind := "invalid_response"
					if name == "transport" {
						kind = "upstream_error"
					}
					requireKind(t, err, kind)
				}
				_, err := client.Quote(context.Background(), p, nil)
				requireKind(t, err, "circuit_open")
				if calls != 3 {
					t.Fatalf("invalid response did not open breaker: calls=%d", calls)
				}
			})
		})
	}
}