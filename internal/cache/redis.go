package cache

import (
	"context"
	"errors"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/redis/go-redis/v9"
)

var (
	ErrMiss    = errors.New("cache miss")
	ErrExpired = errors.New("cache entry expired")
)

type Redis struct {
	client  *redis.Client
	timeout time.Duration
}

func NewRedis(cfg platform.Cache) (*Redis, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client := redis.NewClient(&redis.Options{
		Addr:                  cfg.Address,
		Protocol:              2,
		MaxRetries:            -1,
		DialTimeout:           cfg.OperationTimeout,
		ReadTimeout:           cfg.OperationTimeout,
		WriteTimeout:          cfg.OperationTimeout,
		PoolTimeout:           cfg.OperationTimeout,
		ContextTimeoutEnabled: true,
		DisableIdentity:       true,
	})
	return &Redis{client: client, timeout: cfg.OperationTimeout}, nil
}

func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	value, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrMiss
	}
	return value, err
}

func (r *Redis) Set(ctx context.Context, key string, value []byte, expiresAt time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	if !expiresAt.After(time.Now()) {
		return ErrExpired
	}
	// Absolute millisecond expiry cannot grant more lifetime after network delay.
	return r.client.Do(ctx, "SET", key, value, "PXAT", expiresAt.UnixMilli()).Err()
}

func (r *Redis) Close() error { return r.client.Close() }