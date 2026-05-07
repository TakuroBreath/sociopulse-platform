package config

// viperDefaulter is the minimal subset of *viper.Viper that seedDefaults uses.
// Stated as an interface so tests can stub it without spinning up a Viper.
type viperDefaulter interface {
	SetDefault(key string, value any)
}

// seedDefaults pushes a Config tree into Viper so missing yaml keys resolve.
// We only seed fields whose zero value would fail Validate or surprise an
// operator — sub-trees with no validation (S3, KMS, recording, etc.) are
// intentionally left for the YAML to populate.
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
}
