package provider

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Config is what a registry constructor receives: the endpoint row in the
// shape a provider needs. Provider selects the adapter; the rest is that
// adapter's connection. Timeout bounds one request and defaults to the
// adapter's own when zero. HTTPClient, when set, is used instead of a fresh
// client, so a test can point a provider at a fake transport.
type Config struct {
	Provider   string
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	Timeout    time.Duration
	Reach      string
}

// Registry maps the endpoints.provider value to the adapter that speaks it.
// Adding a protocol is one file plus one Register line in NewRegistry, and
// nothing else in the system changes: placement, leases and budgets speak
// capabilities, never provider names (spec §10).
//
// The zero value is usable and empty; NewRegistry is the registry with every
// adapter this binary ships.
type Registry struct {
	mu    sync.RWMutex
	ctors map[string]func(Config) (Provider, error)
}

// NewRegistry returns a registry holding every adapter this binary ships. The
// list is deliberately here and nowhere else: one line per protocol.
func NewRegistry() *Registry {
	r := &Registry{}
	r.Register("stub", newStubProvider)
	r.Register("openai", newOpenAIProvider)
	return r
}

// Register adds one adapter under name. Registration is a wiring decision made
// before the process serves anything, so a bad name, a nil constructor or a
// duplicate is a programming error and panics: Register's signature cannot
// return an error (the brief fixes it), and a silent overwrite would leave one
// adapter unreachable with no signal at all.
//
// Names are matched exactly against endpoints.provider, so anything but a
// lowercase, whitespace-free, printable name is rejected here rather than
// becoming an adapter the board can never ask for (checkName).
func (r *Registry) Register(name string, ctor func(Config) (Provider, error)) {
	if err := checkName(name); err != nil {
		panic("provider: " + err.Error())
	}
	if ctor == nil {
		panic(fmt.Sprintf("provider: register %q: nil constructor", name))
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctors == nil {
		r.ctors = make(map[string]func(Config) (Provider, error))
	}
	if _, dup := r.ctors[name]; dup {
		panic(fmt.Sprintf("provider: register %q: already registered", name))
	}
	r.ctors[name] = ctor
}

// checkName rejects a name the board could never emit. endpoints.provider is
// compared by exact string equality, so "OpenAI", " openai" and "openai " all
// describe an adapter no endpoint row can select — and the symptom would be
// "unknown provider" on every claim, naming the symptom rather than the typo.
//
// The rule is a charset rather than a trim-and-lowercase: an invisible
// character (a non-breaking space, a zero-width space pasted from a chat) makes
// a name nobody can retype, and every spelling that merely looks canonical is
// the same trap.
func checkName(name string) error {
	if name == "" {
		return errors.New("empty provider name")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return fmt.Errorf("provider name %q: character %q is not allowed (want lowercase letters, digits, '.', '-' or '_')", name, r)
		}
	}
	return nil
}

// New constructs the provider named by cfg.Provider. The name is matched
// exactly: a typo, a different case or stray whitespace is an error naming the
// request and listing what is registered, because an endpoint whose provider
// cannot be constructed fails every run placed on it.
func (r *Registry) New(cfg Config) (Provider, error) {
	if cfg.Provider == "" {
		return nil, errors.New("provider: no provider name in config")
	}

	r.mu.RLock()
	ctor, ok := r.ctors[cfg.Provider]
	r.mu.RUnlock()
	if !ok {
		// The names are read after the lock is released. Go's RWMutex does not
		// allow a second RLock while a writer is waiting, and a names list that
		// only formats an error must never be able to deadlock the registry.
		names := r.Names()
		if len(names) == 0 {
			return nil, fmt.Errorf("provider: unknown provider %q (nothing is registered)", cfg.Provider)
		}
		return nil, fmt.Errorf("provider: unknown provider %q (registered: %s)",
			cfg.Provider, strings.Join(names, ", "))
	}

	p, err := ctor(cfg)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", cfg.Provider, err)
	}
	if p == nil {
		return nil, fmt.Errorf("provider %q: constructor returned no provider", cfg.Provider)
	}
	// A provider's Name is what dispatch and the endpoint row believe they are
	// talking to. A copy-pasted Name() that disagrees with its registration key
	// would be invisible until the wrong adapter ran, so the first construction
	// checks it. Register cannot: it holds only a function, and running it
	// without a real endpoint configuration would test the constructor against a
	// lie.
	if name := p.Name(); name != cfg.Provider {
		return nil, fmt.Errorf("provider %q: constructor returned a provider named %q", cfg.Provider, name)
	}
	return p, nil
}

// Names returns the registered names, sorted so listings and error messages are
// stable. It returns a copy: mutating it cannot change the registry.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.ctors))
	for name := range r.ctors {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
