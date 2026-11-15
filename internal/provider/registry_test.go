package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// namedStub is the stub under a different name. New requires a constructor to
// return a provider that reports the name it was registered under, so a test
// that registers "openai" needs a provider whose Name() is "openai".
type namedStub struct {
	*Stub
	name string
}

func (n namedStub) Name() string { return n.name }

// ctorFor builds the shape a real adapter has: Name() and the registration key
// agree.
func ctorFor(name string) func(Config) (Provider, error) {
	return func(Config) (Provider, error) { return namedStub{Stub: NewStub(nil), name: name}, nil }
}

// stubCtor is a valid constructor that needs no endpoint. The registry tests do
// not care what it builds, only that a name is taken.
func stubCtor(Config) (Provider, error) { return NewStub(nil), nil }

// mustPanic runs fn and returns what panic produced, failing the test if fn
// returns normally. Registration is the one place this package panics: a
// duplicate that merely overwrote the first adapter would be invisible, and the
// brief binds Register to a signature that cannot return an error.
func mustPanic(t *testing.T, fn func()) (recovered any) {
	t.Helper()
	defer func() { recovered = recover() }()
	fn()
	t.Fatalf("expected a panic, but the call returned")
	return nil
}

func countName(names []string, name string) int {
	n := 0
	for _, got := range names {
		if got == name {
			n++
		}
	}
	return n
}

func TestRegistryRejectsUnknownProvider(t *testing.T) {
	r := NewRegistry()

	p, err := r.New(Config{Provider: "not-a-provider"})
	if err == nil {
		t.Fatalf("New(%q) = %v, nil; want an unknown-provider error", "not-a-provider", p)
	}
	if p != nil {
		t.Fatalf("New(%q) returned a provider with an error: %v", "not-a-provider", p)
	}
	if !strings.Contains(err.Error(), "not-a-provider") {
		t.Fatalf("error %q does not name the provider the caller asked for", err)
	}

	// An empty name is its own mistake: the caller never chose an adapter.
	if _, err := r.New(Config{}); err == nil {
		t.Fatal("New(Config{}) = nil error; want a missing-name error")
	}
}

func TestRegistryRejectsDuplicateRegistration(t *testing.T) {
	// The zero-value registry, not NewRegistry: the name under test is the
	// shipped "openai" adapter, so using the shipped registry would make this a
	// test of which adapters ship rather than of duplicate registration.
	r := &Registry{}
	openai := ctorFor("openai")
	r.Register("openai", openai)

	recovered := mustPanic(t, func() { r.Register("openai", openai) })
	if msg := fmt.Sprint(recovered); !strings.Contains(msg, "openai") {
		t.Fatalf("duplicate-registration panic %q does not name the provider", msg)
	}

	// The failed registration must not have replaced or duplicated the first
	// one: exactly one entry, and it still constructs a provider.
	if got := countName(r.Names(), "openai"); got != 1 {
		t.Fatalf("after the duplicate, Names() has %d entries for openai, want 1: %v", got, r.Names())
	}
	p, err := r.New(Config{Provider: "openai"})
	if err != nil || p == nil {
		t.Fatalf("New(openai) after a rejected duplicate = %v, %v; the first registration is gone", p, err)
	}
}

func TestRegistryRegisterRejectsNamesTheBoardCannotMatch(t *testing.T) {
	// endpoints.provider is matched by exact string equality, and the board
	// stores what the operator typed. A name that is not canonical can never be
	// emitted, so registering it would create an adapter that is unreachable
	// while every claim reports "unknown provider" — a message that names the
	// symptom, not the typo.
	for _, name := range []string{"", " openai", "openai ", "OpenAI", "open ai", "openai\n", "open\u00a0ai", "openai/"} {
		t.Run(fmt.Sprintf("%q", name), func(t *testing.T) {
			r := NewRegistry()
			recovered := mustPanic(t, func() { r.Register(name, stubCtor) })
			if recovered == nil {
				return // mustPanic already failed
			}
			// The message quotes the name, so a control character appears
			// escaped: compare against the quoted spelling.
			if msg := fmt.Sprint(recovered); name != "" && !strings.Contains(msg, fmt.Sprintf("%q", name)) {
				t.Fatalf("panic %q does not name the rejected provider %q", msg, name)
			}
		})
	}

	// And construction is exact too: a differently-spelled name does not alias
	// a registered one. Silently resolving "STUB" here is how a typo becomes a
	// run against the wrong endpoint.
	for _, name := range []string{"STUB", " stub", "stub ", "Stub"} {
		if _, err := NewRegistry().New(Config{Provider: name}); err == nil {
			t.Errorf("New(%q) resolved a registered name; names are matched exactly", name)
		}
	}
}

func TestRegistryNamesAreSorted(t *testing.T) {
	r := NewRegistry()
	r.Register("zeta", stubCtor)
	r.Register("alpha", stubCtor)

	names := r.Names()
	// The shipped adapters are the stub, anthropic, openai and ollama; alpha
	// and zeta are this test's own registrations. Each shipped name being here is the one
	// registry line a protocol task adds, and this assertion is what notices its
	// loss.
	want := []string{"alpha", "anthropic", "ollama", "openai", "stub", "zeta"}
	if len(names) != len(want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("Names() = %v, want %v (sorted, so listings and error messages are stable)", names, want)
		}
	}

	// Names is a copy: a caller sorting or truncating it cannot change the
	// registry.
	names[0] = "mutated"
	if got := r.Names()[0]; got != "alpha" {
		t.Fatalf("Names() exposes the registry's own slice: got %q after mutating the returned slice", got)
	}
}

func TestRegistryIsSafeForConcurrentUse(t *testing.T) {
	// Endpoint health probes construct providers concurrently, and endpoint CRUD
	// may register while a probe runs. -race is the assertion that matters here;
	// the loops exist to give it interleavings to see.
	r := NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			r.Register(fmt.Sprintf("extra-%d", i), stubCtor)
		}(i)
		go func() {
			defer wg.Done()
			p, err := r.New(Config{Provider: "stub"})
			if err != nil || p == nil {
				t.Errorf("New(stub) = %v, %v", p, err)
			}
			_ = r.Names()
		}()
	}
	wg.Wait()

	// 16 concurrent extras plus the four shipped adapters (stub, anthropic,
	// openai, ollama).
	if len(r.Names()) != 20 {
		t.Fatalf("Names() = %v, want 20 entries", r.Names())
	}
	// The registry is still usable after the stampede.
	if _, err := r.New(Config{Provider: "stub"}); err != nil {
		t.Fatalf("New(stub) after concurrent use: %v", err)
	}
	if p, err := NewRegistry().New(Config{Provider: "stub"}); err != nil || p == nil || p.Name() != "stub" {
		t.Fatalf("New(stub) = %v, %v; want a stub", p, err)
	}
}

func TestRegistryNewRejectsConstructorWhoseNameDisagrees(t *testing.T) {
	// The copy-paste mistake a real adapter makes: the file is registered as
	// "openai" but its Name() method still says "stub". Nothing before the first
	// construction can see it, and by then dispatch would be pointed at the
	// wrong protocol. A zero-value registry keeps this about the name assertion
	// rather than about which adapters ship.
	r := &Registry{}
	r.Register("openai", stubCtor) // the stub reports "stub"

	_, err := r.New(Config{Provider: "openai"})
	if err == nil {
		t.Fatal("New(openai) accepted a provider whose Name() is \"stub\"; dispatch would reach the wrong adapter")
	}
	for _, want := range []string{"openai", "stub"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
}

func TestRegistryCarriesTheEndpointReachToTheProvider(t *testing.T) {
	// The endpoint row is where the declared reach lives; a provider built from
	// it must still refuse a request whose own Reach says host. This is the
	// wiring between Config and the adapter's half of checkReach.
	p, err := NewRegistry().New(Config{Provider: "stub", Reach: "wan"})
	if err != nil {
		t.Fatalf("New(stub, reach wan): %v", err)
	}
	if _, err := p.Complete(context.Background(), Request{Model: "m", RequireLocal: true, Reach: "host"}); !errors.Is(err, ErrEgressRefused) {
		t.Fatalf("stub built from a wan endpoint accepted a host-declared local-only request: %v", err)
	}
}
