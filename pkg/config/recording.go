package config

import "time"

// RecordingConfig — Plan 12 owns the pipeline; we only plumb settings here.
type RecordingConfig struct {
	LocalBufferPath string             `mapstructure:"local_buffer_path"`
	StagingPath     string             `mapstructure:"staging_path"`
	FFmpeg          RecordingFFmpeg    `mapstructure:"ffmpeg"`
	Upload          RecordingUpload    `mapstructure:"upload"`
	Retention       RecordingRetention `mapstructure:"retention"`
}

// RecordingFFmpeg is the encoder configuration the local recorder hands to
// the ffmpeg process.
type RecordingFFmpeg struct {
	Codec      string `mapstructure:"codec"`
	Bitrate    string `mapstructure:"bitrate"`
	SampleRate int    `mapstructure:"sample_rate"`
}

// RecordingUpload tunes the retry behaviour when the uploader cannot reach
// object storage.
type RecordingUpload struct {
	RetryInitialDelay time.Duration `mapstructure:"retry_initial_delay"`
	RetryMaxDelay     time.Duration `mapstructure:"retry_max_delay"`
	RetryMaxAttempts  int           `mapstructure:"retry_max_attempts"`
}

// RecordingRetention controls hot/cold storage tier transitions.
type RecordingRetention struct {
	DefaultHotDays   int    `mapstructure:"default_hot_days"`
	DefaultColdDays  int    `mapstructure:"default_cold_days"`
	ColdStorageClass string `mapstructure:"cold_storage_class"`
}
