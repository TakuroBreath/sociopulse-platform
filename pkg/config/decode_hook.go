package config

import (
	"reflect"

	"github.com/go-viper/mapstructure/v2"
)

// decodeHook returns a composed Viper decode hook that recognises:
//   - Go duration strings ("15m", "30s") → time.Duration
//   - K/M/G size suffixes ("10MB", "1GB") → int64 bytes
//   - comma-separated strings → []string
func decodeHook() mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
		stringToBytesHookFunc(),
	)
}

func stringToBytesHookFunc() mapstructure.DecodeHookFunc {
	return func(f reflect.Type, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String {
			return data, nil
		}
		if t.Kind() != reflect.Int64 && t.Kind() != reflect.Int {
			return data, nil
		}
		s, ok := data.(string)
		if !ok {
			return data, nil
		}
		// Only handle suffixes — plain integers are decoded by mapstructure already.
		var mult int64
		switch {
		case len(s) > 2 && s[len(s)-2:] == "KB":
			mult = 1024
			s = s[:len(s)-2]
		case len(s) > 2 && s[len(s)-2:] == "MB":
			mult = 1024 * 1024
			s = s[:len(s)-2]
		case len(s) > 2 && s[len(s)-2:] == "GB":
			mult = 1024 * 1024 * 1024
			s = s[:len(s)-2]
		default:
			return data, nil
		}
		var n int64
		for _, ch := range s {
			if ch < '0' || ch > '9' {
				return data, nil
			}
			n = n*10 + int64(ch-'0')
		}
		return n * mult, nil
	}
}
