package cache

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestRedisStoresWithAtomicAbsoluteExpiry(t *testing.T) {
	server := miniredis.RunT(t)
	now := time.Now().UTC()
	server.SetTime(now)
	store, err := NewRedis(platform.Cache{Address: server.Addr(), TTL: 300 * time.Second, OperationTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrMiss) {
		t.Fatalf("miss error %v", err)
	}
	expiry := now.Add(1250 * time.Millisecond)
	if err := store.Set(ctx, "isolated-test-key", []byte("original"), expiry); err != nil {
		t.Fatal(err)
	}
	if ttl := server.TTL("isolated-test-key"); ttl != expiry.Truncate(time.Millisecond).Sub(now) {
		t.Fatalf("wrong absolute TTL: %s", ttl)
	}
	value, err := store.Get(ctx, "isolated-test-key")
	if err != nil || string(value) != "original" {
		t.Fatalf("cache read: %s %v", value, err)
	}
	if err := store.Set(ctx, "isolated-test-key", []byte("expired"), now.Add(-time.Second)); !errors.Is(err, ErrExpired) {
		t.Fatalf("past expiry accepted: %v", err)
	}
	server.FastForward(2 * time.Second)
	if _, err := store.Get(ctx, "isolated-test-key"); !errors.Is(err, ErrMiss) {
		t.Fatalf("expired entry still available: %v", err)
	}
}

func TestRedisDeadlineIncludesDialHandshakeAndCommandWithoutRetries(t *testing.T) {
	for _, stage := range []string{"dial", "handshake", "command"} {
		for _, operation := range []string{"get", "set"} {
			t.Run(stage+"/"+operation, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store, err := NewRedis(platform.Cache{
						Address: "unused:6379", TTL: 300 * time.Second, OperationTimeout: 20 * time.Millisecond,
					})
					if err != nil {
						t.Fatal(err)
					}
					_ = store.Close()
					options := *store.client.Options()
					var dials, commands atomic.Int32
					options.Dialer = func(ctx context.Context, _, _ string) (net.Conn, error) {
						dials.Add(1)
						if stage == "dial" {
							<-ctx.Done()
							return nil, ctx.Err()
						}
						time.Sleep(5 * time.Millisecond)
						client, peer := net.Pipe()
						go func() {
							defer peer.Close()
							reader := bufio.NewReader(peer)
							for {
								args, err := readCommand(reader)
								if err != nil {
									return
								}
								switch strings.ToLower(args[0]) {
								case "hello":
									if stage == "handshake" {
										_, _ = io.Copy(io.Discard, peer)
										return
									}
									time.Sleep(5 * time.Millisecond)
									_, _ = io.WriteString(peer, "*0\r\n")
								case "get", "set":
									commands.Add(1)
									_, _ = io.Copy(io.Discard, peer)
									return
								default:
									_, _ = io.WriteString(peer, "+OK\r\n")
								}
							}
						}()
						return client, nil
					}
					store.client = redis.NewClient(&options)
					defer store.Close()
					start := time.Now()
					if operation == "get" {
						_, err = store.Get(context.Background(), "key")
					} else {
						err = store.Set(context.Background(), "key", []byte("value"), start.Add(time.Minute))
					}
					if err == nil || time.Since(start) != 20*time.Millisecond || dials.Load() != 1 {
						t.Fatalf("deadline/retry violated: elapsed=%v dials=%d err=%v", time.Since(start), dials.Load(), err)
					}
					if (stage == "command" && commands.Load() != 1) || commands.Load() > 1 {
						t.Fatalf("command was skipped or retried: %d", commands.Load())
					}
				})
			})
		}
	}
}

func TestRedisDeadlineIncludesPoolWaitAndHonorsEarlierParentDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store, err := NewRedis(platform.Cache{
			Address: "unused:6379", TTL: 300 * time.Second, OperationTimeout: 20 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = store.Close()
		options := *store.client.Options()
		options.PoolSize, options.MaxActiveConns = 1, 1
		options.Dialer = func(context.Context, string, string) (net.Conn, error) {
			client, peer := net.Pipe()
			go func() {
				defer peer.Close()
				reader := bufio.NewReader(peer)
				for {
					args, err := readCommand(reader)
					if err != nil {
						return
					}
					if strings.EqualFold(args[0], "hello") {
						_, _ = io.WriteString(peer, "*0\r\n")
					} else {
						_, _ = io.WriteString(peer, "+PONG\r\n")
					}
				}
			}()
			return client, nil
		}
		store.client = redis.NewClient(&options)
		defer store.Close()
		held := store.client.Conn()
		defer held.Close()
		if err := held.Ping(context.Background()).Err(); err != nil {
			t.Fatal(err)
		}
		for _, deadline := range []time.Duration{20 * time.Millisecond, 5 * time.Millisecond} {
			ctx, cancel := context.WithTimeout(context.Background(), deadline)
			start := time.Now()
			_, err := store.Get(ctx, "key")
			cancel()
			if err == nil || time.Since(start) != deadline {
				t.Fatalf("pool wait ignored total/parent deadline: elapsed=%v err=%v", time.Since(start), err)
			}
		}
	})
}

func readCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "*")))
	if err != nil || count <= 0 {
		return nil, fmt.Errorf("invalid RESP array: %q", line)
	}
	args := make([]string, count)
	for i := range args {
		line, err = reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "$")))
		if err != nil || size < 0 {
			return nil, fmt.Errorf("invalid RESP string: %q", line)
		}
		payload := make([]byte, size+2)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return nil, err
		}
		args[i] = string(payload[:size])
	}
	return args, nil
}