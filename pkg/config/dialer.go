package config

import "time"

// DialerConfig holds defaults for the auto-dialer. Per-tenant overrides live
// in tenant_settings (see spec §14.3).
type DialerConfig struct {
	Defaults DialerDefaults `mapstructure:"defaults"`
}

// DialerDefaults is the platform-wide fallback for dialer behaviour. Per-tenant
// overrides in tenant_settings take precedence at runtime.
type DialerDefaults struct {
	AttemptMax            int           `mapstructure:"attempt_max"`
	RetryNoAnswerDelay    time.Duration `mapstructure:"retry_no_answer_delay"`
	RetryBusyDelay        time.Duration `mapstructure:"retry_busy_delay"`
	RetryDroppedDelay     time.Duration `mapstructure:"retry_dropped_delay"`
	RetryTechFailureDelay time.Duration `mapstructure:"retry_tech_failure_delay"`
	DialingTimeout        time.Duration `mapstructure:"dialing_timeout"`
	PauseMax              time.Duration `mapstructure:"pause_max"`
	RDD                   RDDConfig     `mapstructure:"rdd"`
	WorkingHours          WorkingHours  `mapstructure:"working_hours"`
}

// RDDConfig governs random-digit-dialling generation.
type RDDConfig struct {
	Enabled            bool    `mapstructure:"enabled"`
	MaxRatePerSec      int     `mapstructure:"max_rate_per_sec"`
	FallbackThreshold  float64 `mapstructure:"fallback_threshold"`
	MaxAttemptsPerCall int     `mapstructure:"max_attempts_per_call"`
}

// WorkingHours partitions weekday and weekend dialling windows.
type WorkingHours struct {
	Weekdays HoursWindow `mapstructure:"weekdays"`
	Weekends HoursWindow `mapstructure:"weekends"`
}

// HoursWindow is a "HH:MM" pair describing an inclusive dialling window.
type HoursWindow struct {
	From string `mapstructure:"from"`
	To   string `mapstructure:"to"`
}
