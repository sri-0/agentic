package redis

import (
	"context"
	"fmt"

	"agentic/pkg/logging"

	"github.com/redis/go-redis/v9"
)

type RedisConfig struct {
	Host string `env:"REDIS_HOST, default=localhost"`
	Port int    `env:"REDIS_PORT, default=6379"`
	User string `env:"REDIS_USER, default=default"`
	Pass string `env:"REDIS_PASS"`
	Db   int    `env:"REDIS_DB, default=0"`
}

type Redis struct {
	Client *redis.Client
}

func New(ctx context.Context, config RedisConfig) (*Redis, error) {
	l := logging.Get(ctx)
	opts := &redis.Options{
		Addr:     fmt.Sprintf("%s:%d", config.Host, config.Port),
		Username: config.User,
		Password: config.Pass,
		DB:       config.Db,
	}

	l.Info().Str("addr", opts.Addr).Str("user", opts.Username).Int("db", opts.DB).Msg("redis: configuring client")
	client := redis.NewClient(opts)

	out, err := client.Ping(ctx).Result()
	if err != nil {
		l.Err(err).Str("addr", opts.Addr).Msg("redis: failed to connect")
		return nil, err
	}
	l.Info().Str("pong", out).Msg("redis: connected")

	return &Redis{Client: client}, nil
}
