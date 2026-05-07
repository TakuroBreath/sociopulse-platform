# internal/

Private Go packages — importable only by this module.

## Module boundary rule

Cross-module imports must go through `internal/<module>/api/` only.
Importing another module's `service`, `store`, or `events` is a lint error.
Enforced by depguard (see .golangci.yml).
