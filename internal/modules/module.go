// Package modules defines the registration pattern used by cmd/api to
// compose all business modules into running servers.
//
// A Module is a self-registering unit — Plan 02's cmd/api walks a list of
// Modules and calls Register on each. Each module's Register builds its
// store, service, HTTP handlers, gRPC servers, and NATS subscribers from
// the curated Deps the composition root provides.
package modules

import (
	"context"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"google.golang.org/grpc"

	"github.com/sociopulse/platform/pkg/config"
	"github.com/sociopulse/platform/pkg/eventbus"
	"github.com/sociopulse/platform/pkg/postgres"
)

// Deps is what every module receives at registration. It is the curated set
// of cross-cutting dependencies the composition root knows how to build.
//
// Modules must NOT reach beyond Deps for shared infrastructure — adding a
// new field here is intentional and reviewed.
type Deps struct {
	Ctx    context.Context
	Logger *zap.Logger
	Config *config.Config
	Pool   *postgres.Pool
	// Redis is the project-wide Redis client used for refresh-token
	// whitelisting, session revocation, login rate-limiting, and account
	// lockout. Plan 05 Task 8/9 introduced this field; cmd/api wires a
	// real *redis.Client once the host environment exposes one — until
	// then individual modules guard nil and skip features that depend
	// on Redis. The redis.UniversalClient interface is used (rather
	// than *redis.Client) so the same wiring works against
	// *redis.ClusterClient, miniredis, and Sentinel backends without
	// reshuffling Deps.
	Redis      redis.UniversalClient
	EventBus   eventbus.Publisher
	Subscriber eventbus.Subscriber
	HTTPRouter *gin.Engine
	GRPCServer *grpc.Server
	Locator    ServiceLocator
}

// ServiceLocator is the explicit registry for cross-module API references.
// Modules register their api.* implementations here at startup; downstream
// modules look them up by interface type at their own Register-time.
//
// This pattern is used instead of compile-time DI to avoid cycles when two
// modules reference each other through interfaces — and to keep cmd/api's
// composition root flat (no N×M hand-wired struct fields).
//
// Lookups by name (string) are intentionally typed: callers do
//
//	rawSvc, ok := d.Locator.Lookup("auth.AuthService")
//	if !ok { return errors.New("modules: auth.AuthService not registered") }
//	authSvc, ok := rawSvc.(authapi.AuthService)
//	if !ok { return errors.New("modules: auth.AuthService wrong type") }
//
// — names use the dotted "<module>.<TypeName>" convention.
type ServiceLocator interface {
	Register(name string, svc any)
	Lookup(name string) (any, bool)
}

// Module is implemented by each internal/<name>/module.go. cmd/api iterates
// the registry, calling Register on each module in dependency order (audit →
// tenancy → ... → realtime). A module's Name() must be unique within the
// registry.
type Module interface {
	Name() string
	Register(d Deps) error
}

// Registry holds the ordered list of modules to register. Order matters:
// modules later in the list may look up services registered earlier.
type Registry struct {
	Modules []Module
}
