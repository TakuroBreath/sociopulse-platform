package config

import "time"

// TelephonyConfig — bridge endpoints + trunk catalog + routing. Plan 09 fills
// the bridge logic; cmd/api just plumbs.
type TelephonyConfig struct {
	Bridge  TelephonyBridgeConfig `mapstructure:"bridge"`
	Trunks  []TrunkConfig         `mapstructure:"trunks"`
	Routing TelephonyRouting      `mapstructure:"routing"`
}

// TelephonyBridgeConfig describes the FreeSWITCH bridge fleet cmd/api drives.
type TelephonyBridgeConfig struct {
	FSNodes              []FSNode      `mapstructure:"fs_nodes"`
	HealthcheckInterval  time.Duration `mapstructure:"healthcheck_interval"`
	MaxConcurrentPerNode int           `mapstructure:"max_concurrent_per_node"`
}

// FSNode is one FreeSWITCH node — ESL endpoint plus the mTLS material to
// authenticate against it.
type FSNode struct {
	ID          string `mapstructure:"id"`
	ESLEndpoint string `mapstructure:"esl_endpoint"`
	ESLCert     string `mapstructure:"esl_cert"`
	ESLKey      string `mapstructure:"esl_key"`
}

// TrunkConfig describes a SIP trunk; the dialer's least-cost router reads
// this list at startup.
type TrunkConfig struct {
	ID               string           `mapstructure:"id"`
	SIPGateway       string           `mapstructure:"sip_gateway"`
	CapacityChannels int              `mapstructure:"capacity_channels"`
	CostPerMinuteRub float64          `mapstructure:"cost_per_minute_rub"`
	Weight           int              `mapstructure:"weight"`
	Regions          []string         `mapstructure:"regions"`
	Healthcheck      TrunkHealthCheck `mapstructure:"healthcheck"`
}

// TrunkHealthCheck describes how the bridge probes a trunk's liveness.
type TrunkHealthCheck struct {
	Method         string        `mapstructure:"method"`
	Interval       time.Duration `mapstructure:"interval"`
	Timeout        time.Duration `mapstructure:"timeout"`
	UnhealthyAfter int           `mapstructure:"unhealthy_after"`
}

// TelephonyRouting names the default trunk-selection strategy.
type TelephonyRouting struct {
	DefaultStrategy string `mapstructure:"default_strategy"`
}
