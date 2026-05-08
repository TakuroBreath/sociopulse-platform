package main

import (
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/sociopulse/platform/internal/modules"
)

// registerModules walks the registry and calls Register on each
// module. A module that requires Redis but Redis is unreachable
// (redisErr != nil) is skipped with a warning so cmd/api still boots
// in test environments without a Redis container.
//
// The function intentionally does NOT short-circuit on the first
// module-Register error — modules degrade gracefully (e.g. dialer
// returns an error only when its truly-required deps are missing,
// not for optional locator entries). A genuine error here surfaces
// as a Warn + skip rather than failing the whole boot, since cmd/api
// must remain useful for /healthz / /metrics / /readyz even when
// individual business modules can't initialise.
//
// redisErr non-nil ⇒ a hard skip for any module whose Register would
// require *Deps.Redis. Today that's the dialer module; the test
// environment runs without Redis so we tolerate the skip. Production
// pings will succeed and every module registers normally.
func registerModules(reg modules.Registry, deps modules.Deps, logger *zap.Logger, redisErr error) error {
	for _, mod := range reg.Modules {
		if mod == nil {
			continue
		}
		if redisErr != nil && requiresRedis(mod) {
			logger.Warn("skipping module: Redis unreachable",
				zap.String("module", mod.Name()),
				zap.Error(redisErr))
			continue
		}
		if err := mod.Register(deps); err != nil {
			// A truly required-dep failure here (e.g. Logger nil)
			// indicates a wiring bug; surface but keep going so the
			// HTTP layer comes up. dialer/auth/crm all return
			// structured errors that include the missing dep name.
			logger.Warn("module Register failed",
				zap.String("module", mod.Name()),
				zap.Error(err))
			// Track the first error so a downstream caller can
			// inspect; today nobody does so we discard.
			if errors.Is(err, errFatalRegistration) {
				return fmt.Errorf("registerModules: %s: %w", mod.Name(), err)
			}
		}
	}
	return nil
}

// errFatalRegistration is reserved for modules that explicitly
// declare a registration failure as fatal-to-boot. None today —
// every module degrades gracefully — but the seam exists so a
// future module (e.g. one that holds the only signing key) can flag
// itself as load-bearing.
var errFatalRegistration = errors.New("module registration is fatal")

// requiresRedis reports whether a module's Register would dereference
// d.Redis. Today only the dialer module does so; auth would but auth
// is not yet on the cmd/api registry. The list is hand-maintained —
// Plan 11 adds the realtime module, which also needs Redis.
//
// Rationale: rather than threading a boolean through every Module's
// Register and risking a panic on a missing Redis, we declare the
// dependency at the registry level. Modules whose Register
// gracefully degrades on missing Redis (e.g. surveys, which doesn't
// touch Redis) are NOT on this list.
func requiresRedis(mod modules.Module) bool {
	switch mod.Name() {
	case "dialer":
		return true
	default:
		return false
	}
}
