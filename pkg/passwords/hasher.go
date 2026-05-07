package passwords

// Hasher is the small interface the rest of the codebase consumes for
// password operations. Tests in dependent packages can substitute a fake
// (e.g. SHA-1-of-password) so unit tests don't pay the 64 MiB Argon2 cost.
//
// Per project convention (07-go-coding-standards §5), the interface is
// defined here at the producer because every consumer wants the same two
// methods; redeclaring it at each consumer would invite drift. Constructors
// still return the concrete type — the interface is purely a seam for
// dependency injection in tests.
type Hasher interface {
	// Hash derives a fresh PHC-encoded Argon2id hash of password using
	// the parameters the implementation was constructed with.
	Hash(password string) (string, error)

	// Verify reports whether password matches the embedded key in
	// encoded. (false, ErrInvalidHash) on a malformed encoded string.
	Verify(encoded, password string) (bool, error)
}

// defaultHasher is the production-ready Hasher backed by package-level
// Hash/Verify and a pre-baked Params. It is a value receiver type because
// it carries no mutable state — Params is a tiny POD copy.
type defaultHasher struct {
	p Params
}

// Compile-time interface conformance check. If we ever drift the Hasher
// interface (add a method, change a signature) this line stops the build
// with a clear message — much friendlier than a runtime "method missing"
// somewhere downstream.
var _ Hasher = defaultHasher{}

// Default returns a Hasher with DefaultParams() baked in. This is the
// convention call-sites should use unless they explicitly need to tune
// the parameters (which warrants a code-review conversation per spec §14.2).
func Default() Hasher {
	return defaultHasher{p: DefaultParams()}
}

// NewHasher returns a Hasher using the given Params. Use this when calling
// code needs non-default cost (e.g. tighter parameters in tests). The Params
// are validated lazily — the first Hash call is the one that surfaces an
// invalid-Params error.
func NewHasher(p Params) Hasher {
	return defaultHasher{p: p}
}

// Hash satisfies Hasher by delegating to the package-level Hash function.
func (d defaultHasher) Hash(password string) (string, error) {
	return Hash(password, d.p)
}

// Verify satisfies Hasher by delegating to the package-level Verify
// function. The constructor's Params are intentionally NOT consulted —
// every PHC string carries its own parameters and we honour those.
func (d defaultHasher) Verify(encoded, password string) (bool, error) {
	return Verify(encoded, password)
}
