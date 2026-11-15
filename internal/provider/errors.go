package provider

import "errors"

// The seven classes every provider error normalizes to (design §10). They are
// sentinels rather than a typed error so a caller classifies with errors.Is and
// never parses a message; an adapter wraps them with its own context.
//
// The retry policy — which classes are worth retrying — is deliberately not
// encoded here: it is retry.go's, and the whole point of keeping it separate is
// that context_overflow and auth must not be retried even though transport and
// server_error must (design §10, §16).
//
// ErrEgressRefused and ErrScriptExhausted are not part of this taxonomy: neither
// is a model-protocol failure, and retrying either would repeat the same bytes.
var (
	ErrAuth            = errors.New("auth")
	ErrRateLimited     = errors.New("rate limited")
	ErrContextOverflow = errors.New("context overflow")
	ErrModelMissing    = errors.New("model missing")
	ErrTransport       = errors.New("transport")
	ErrBadRequest      = errors.New("bad request")
	ErrServer          = errors.New("server")
)
