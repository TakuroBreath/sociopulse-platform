package config

import (
	"errors"
	"time"
)

// DatabaseConfig groups the three persistent stores cmd/api talks to.
type DatabaseConfig struct {
	Postgres   PostgresConfig   `mapstructure:"postgres"`
	ClickHouse ClickHouseConfig `mapstructure:"clickhouse"`
	Redis      RedisConfig      `mapstructure:"redis"`
}

// PostgresConfig is the OLTP store. Plan 03 wires the actual pgxpool from this.
type PostgresConfig struct {
	DSN            string        `mapstructure:"dsn"`
	MaxConns       int           `mapstructure:"max_conns"`
	MaxIdleTime    time.Duration `mapstructure:"max_idle_time"`
	StatementCache int           `mapstructure:"statement_cache"`
	MigrationsPath string        `mapstructure:"migrations_path"`
}

// ClickHouseConfig is the OLAP store. Plan 13 wires it.
type ClickHouseConfig struct {
	DSN           string        `mapstructure:"dsn"`
	BatchSize     int           `mapstructure:"batch_size"`
	FlushInterval time.Duration `mapstructure:"flush_interval"`
}

// RedisConfig governs FSM, queues, presence, idempotency cache, rate-limit buckets.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
	PoolSize int    `mapstructure:"pool_size"`
}

func (d *DatabaseConfig) validate() error {
	if d.Postgres.DSN == "" {
		return errors.New("postgres.dsn required")
	}
	if d.Redis.Addr == "" {
		return errors.New("redis.addr required")
	}
	if d.Postgres.MaxConns <= 0 {
		d.Postgres.MaxConns = 20
	}
	if d.Redis.PoolSize <= 0 {
		d.Redis.PoolSize = 20
	}
	return nil
}
