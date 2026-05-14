package config

import "time"

// viperDefaulter is the minimal subset of *viper.Viper that seedDefaults uses.
// Stated as an interface so tests can stub it without spinning up a Viper.
type viperDefaulter interface {
	SetDefault(key string, value any)
}

// seedDefaults pushes a Config tree into Viper so missing yaml keys resolve.
// We seed every field that Validate requires (and a few quality-of-life
// extras like log_level / metrics namespace) so Load can succeed against an
// empty directory and fall through to DefaultDev. Sub-trees with no
// validation (S3, KMS, telephony, dialer, reports) are intentionally left
// for the YAML to populate; the recording.workers block IS seeded because
// cmd/worker's boot reads them eagerly (Plan 12.4 Task 5).
func seedDefaults(v viperDefaulter, c Config) {
	// service
	v.SetDefault("service.env", c.Service.Env)
	v.SetDefault("service.log_level", c.Service.LogLevel)
	v.SetDefault("service.region", c.Service.Region)
	v.SetDefault("service.name", c.Service.Name)
	// http
	v.SetDefault("http.bind", c.HTTP.Bind)
	v.SetDefault("http.read_timeout", c.HTTP.ReadTimeout)
	v.SetDefault("http.write_timeout", c.HTTP.WriteTimeout)
	v.SetDefault("http.idle_timeout", c.HTTP.IdleTimeout)
	v.SetDefault("http.max_body_size", c.HTTP.MaxBodySize)
	// http.trusted_proxies left unseeded — empty = trust nothing,
	// strictest secure default. Production override via yaml/env.
	// ws
	v.SetDefault("ws.bind", c.WS.Bind)
	v.SetDefault("ws.ping_interval", c.WS.PingInterval)
	v.SetDefault("ws.read_buffer_size", c.WS.ReadBufferSize)
	v.SetDefault("ws.write_buffer_size", c.WS.WriteBufferSize)
	v.SetDefault("ws.max_message_size", c.WS.MaxMessageSize)
	v.SetDefault("ws.handshake_timeout", c.WS.HandshakeTimeout)
	// grpc
	v.SetDefault("grpc.bind", c.GRPC.Bind)
	v.SetDefault("grpc.reflection_enabled", c.GRPC.ReflectionEnabled)
	v.SetDefault("grpc.conn_timeout", c.GRPC.ConnTimeout)
	// database.postgres
	v.SetDefault("database.postgres.dsn", c.Database.Postgres.DSN)
	v.SetDefault("database.postgres.max_conns", c.Database.Postgres.MaxConns)
	v.SetDefault("database.postgres.max_idle_time", c.Database.Postgres.MaxIdleTime)
	v.SetDefault("database.postgres.statement_cache", c.Database.Postgres.StatementCache)
	// database.redis
	v.SetDefault("database.redis.addr", c.Database.Redis.Addr)
	v.SetDefault("database.redis.password", c.Database.Redis.Password)
	v.SetDefault("database.redis.pool_size", c.Database.Redis.PoolSize)
	v.SetDefault("database.redis.db", c.Database.Redis.DB)
	// nats
	v.SetDefault("nats.urls", c.NATS.URLs)
	v.SetDefault("nats.account", c.NATS.Account)
	// auth.jwt
	v.SetDefault("auth.jwt.issuer", c.Auth.JWT.Issuer)
	v.SetDefault("auth.jwt.access_ttl", c.Auth.JWT.AccessTTL)
	v.SetDefault("auth.jwt.refresh_ttl", c.Auth.JWT.RefreshTTL)
	v.SetDefault("auth.jwt.algorithm", c.Auth.JWT.Algorithm)
	// observability
	v.SetDefault("observability.otel.endpoint", c.Observability.OTel.Endpoint)
	v.SetDefault("observability.otel.sampling_ratio", c.Observability.OTel.SamplingRatio)
	v.SetDefault("observability.otel.insecure", c.Observability.OTel.Insecure)
	v.SetDefault("observability.otel.service_name", c.Observability.OTel.ServiceName)
	v.SetDefault("observability.metrics.bind", c.Observability.Metrics.Bind)
	v.SetDefault("observability.metrics.namespace", c.Observability.Metrics.Namespace)
	v.SetDefault("observability.logging.redact_patterns", c.Observability.Logging.RedactPatterns)
	v.SetDefault("observability.logging.sample_info_logs", c.Observability.Logging.SampleInfoLogs)
	v.SetDefault("observability.logging.sample_debug_logs", c.Observability.Logging.SampleDebugLogs)
	// shutdown
	v.SetDefault("shutdown.grace_period", c.Shutdown.GracePeriod)
	// outbox
	v.SetDefault("outbox.batch_size", c.Outbox.BatchSize)
	v.SetDefault("outbox.tick", c.Outbox.Tick)
	v.SetDefault("outbox.max_retry", c.Outbox.MaxRetry)
	// kms — provider-specific fields are optional in YAML; the local
	// fallback is the dev default.
	v.SetDefault("kms.provider", string(c.KMS.Provider))
	v.SetDefault("kms.local_key_hex", c.KMS.LocalKeyHex)
	v.SetDefault("kms.endpoint", c.KMS.Endpoint)
	v.SetDefault("kms.folder_id", c.KMS.FolderID)
	v.SetDefault("kms.service_account_key_path", c.KMS.ServiceAccountKeyPath)
	// recording.workers — cmd/worker's retention + integrity daemons.
	// Defaults match Plan 12.4 §8.5 + §9.4 so an operator with an
	// empty recording.workers block in YAML still gets the intended
	// cadence + batch size.
	v.SetDefault("recording.workers.retention_interval", 5*time.Minute)
	v.SetDefault("recording.workers.retention_batch", 100)
	v.SetDefault("recording.workers.integrity_interval", 1*time.Hour)
	v.SetDefault("recording.workers.integrity_batch", 10)
	v.SetDefault("recording.workers.integrity_sample_percent", 1.0)
	// analytics — Plan 13.2 Task 6. Defaults match DefaultDev so an
	// operator with an empty analytics block in YAML still gets the
	// intended batch / flush / cache TTLs. Enabled=true mirrors the
	// dev expectation that the local CH container is reachable.
	v.SetDefault("analytics.enabled", c.Analytics.Enabled)
	v.SetDefault("analytics.batch_size", c.Analytics.BatchSize)
	v.SetDefault("analytics.flush_interval", c.Analytics.FlushInterval)
	v.SetDefault("analytics.dedup_lru_size", c.Analytics.DedupLRUSize)
	v.SetDefault("analytics.cache_short_ttl", c.Analytics.CacheShortTTL)
	v.SetDefault("analytics.cache_long_ttl", c.Analytics.CacheLongTTL)
	v.SetDefault("analytics.long_window_threshold", c.Analytics.LongWindowThreshold)
	v.SetDefault("analytics.queue_group", c.Analytics.QueueGroup)
	v.SetDefault("analytics.drain_timeout", c.Analytics.DrainTimeout)
}
