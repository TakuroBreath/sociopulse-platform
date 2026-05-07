package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// LoadOptions controls Load behaviour.
type LoadOptions struct {
	// Path to the configs directory (e.g. "configs/development"). The loader
	// reads "config.yaml" inside it. Empty = use SOCIOPULSE_CONFIG_DIR env var.
	Dir string
	// EnvPrefix for ENV-variable overrides. Default: "SOCIOPULSE".
	EnvPrefix string
	// HotReload enables fsnotify on config.yaml.
	HotReload bool
}

// Snapshot is an atomically-replaceable holder for the active Config.
// Subscribers receive a fresh value via the channel returned from Subscribe.
//
// When hot-reload is enabled, callers must call Close at process exit to stop
// the underlying fsnotify watcher; otherwise the reload goroutine leaks for
// the lifetime of the process.
type Snapshot struct {
	mu        sync.RWMutex
	value     atomic.Pointer[Config]
	listeners []chan Config

	// closer stops the hot-reload goroutine when called. nil when
	// HotReload is disabled.
	closer func() error
}

// NewSnapshot wraps an initial Config.
func NewSnapshot(c Config) *Snapshot {
	s := &Snapshot{}
	s.value.Store(&c)
	return s
}

// Get returns the current Config. Safe for concurrent use.
func (s *Snapshot) Get() Config {
	return *s.value.Load()
}

// Subscribe registers a listener that receives a fresh Config on every reload.
// The returned channel is buffered (size 1); old values are dropped if the
// listener is slow.
func (s *Snapshot) Subscribe() <-chan Config {
	ch := make(chan Config, 1)
	s.mu.Lock()
	s.listeners = append(s.listeners, ch)
	s.mu.Unlock()
	return ch
}

// Close stops the hot-reload goroutine, if any. Safe to call on a Snapshot
// that was constructed without HotReload — it is a no-op in that case.
// Close is idempotent.
func (s *Snapshot) Close() error {
	s.mu.Lock()
	closer := s.closer
	s.closer = nil
	s.mu.Unlock()
	if closer == nil {
		return nil
	}
	return closer()
}

// replace atomically swaps the active Config and notifies subscribers.
//
// Single-producer assumption: replace MUST be called from a single goroutine
// (currently the reload loop in startHotReload). Concurrent invocations would
// race on the drain-and-resend path used when a listener channel is full. If
// a future change adds a second producer, switch the inner mutex from
// RWMutex.RLock to Mutex.Lock.
func (s *Snapshot) replace(c Config) {
	s.value.Store(&c)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ch := range s.listeners {
		select {
		case ch <- c:
		default:
			// drop oldest
			select {
			case <-ch:
			default:
			}
			ch <- c
		}
	}
}

// Load reads config.yaml from opts.Dir, applies ENV-var overrides, validates,
// and returns a Snapshot. If opts.HotReload is true, fsnotify watches the file
// and updates the snapshot on change.
func Load(opts LoadOptions) (*Snapshot, error) {
	if opts.EnvPrefix == "" {
		opts.EnvPrefix = "SOCIOPULSE"
	}
	if opts.Dir == "" {
		opts.Dir = os.Getenv("SOCIOPULSE_CONFIG_DIR")
	}
	if opts.Dir == "" {
		return nil, errors.New("config dir not set: use LoadOptions.Dir or SOCIOPULSE_CONFIG_DIR env")
	}

	v := viper.New()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(opts.Dir)

	v.SetEnvPrefix(opts.EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Seed defaults from DefaultDev so optional yaml keys still resolve sensibly.
	seedDefaults(v, DefaultDev())

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			return nil, fmt.Errorf("read config: %w", err)
		}
		// File missing: rely entirely on defaults+env. Useful in tests.
	}

	cfg, err := unmarshalAndValidate(v)
	if err != nil {
		return nil, err
	}
	snap := NewSnapshot(cfg)

	if opts.HotReload {
		if err := startHotReload(v, snap, opts.Dir); err != nil {
			return nil, fmt.Errorf("start hot-reload: %w", err)
		}
	}
	return snap, nil
}

// LoadDefault is a convenience wrapper that returns a Snapshot seeded with
// DefaultDev() and no file/env layering. Useful in tests that need a valid
// *Snapshot without any disk fixture.
func LoadDefault() *Snapshot {
	return NewSnapshot(DefaultDev())
}

func unmarshalAndValidate(v *viper.Viper) (Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg, viper.DecodeHook(decodeHook())); err != nil {
		return Config{}, fmt.Errorf("unmarshal: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate: %w", err)
	}
	return cfg, nil
}

func startHotReload(v *viper.Viper, snap *Snapshot, dir string) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("fsnotify new watcher: %w", err)
	}
	if err := watcher.Add(filepath.Clean(dir)); err != nil {
		_ = watcher.Close()
		return fmt.Errorf("fsnotify watch %s: %w", dir, err)
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	go reloadLoop(watcher, v, snap, done, finished)

	// closer signals the goroutine to exit, then waits for it to finish.
	// Closing the watcher first makes Events/Errors observable as closed
	// channels by the loop; closing done is the explicit-shutdown path.
	snap.mu.Lock()
	snap.closer = func() error {
		err := watcher.Close()
		close(done)
		<-finished
		return err
	}
	snap.mu.Unlock()
	return nil
}

func reloadLoop(watcher *fsnotify.Watcher, v *viper.Viper, snap *Snapshot,
	done <-chan struct{}, finished chan<- struct{}) {
	defer close(finished)
	for {
		select {
		case <-done:
			return
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			handleReloadEvent(ev, v, snap)
		case _, ok := <-watcher.Errors:
			if !ok {
				return
			}
		}
	}
}

// handleReloadEvent inspects one fsnotify event and, if it targets the
// active config.yaml, re-reads + re-validates and atomically replaces the
// snapshot. Validation/parse failures are swallowed: the existing snapshot
// stays in place.
func handleReloadEvent(ev fsnotify.Event, v *viper.Viper, snap *Snapshot) {
	if ev.Op&(fsnotify.Write|fsnotify.Create) == 0 {
		return
	}
	if !strings.HasSuffix(ev.Name, "config.yaml") {
		return
	}
	if err := v.ReadInConfig(); err != nil {
		return
	}
	cfg, err := unmarshalAndValidate(v)
	if err != nil {
		return
	}
	snap.replace(cfg)
}
