package provider

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

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
	r := NewRegistry()
	r.Register("openai", stubCtor)

	recovered := mustPanic(t, func() { r.Register("openai", stubCtor) })
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
	want := []string{"alpha", "stub", "zeta"}
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

	if len(r.Names()) != 17 {
		t.Fatalf("Names() = %v, want 17 entries", r.Names())
	}
	// The registry is still usable after the stampede.
	if _, err := r.New(Config{Provider: "stub"}); err != nil {
		t.Fatalf("New(stub) after concurrent use: %v", err)
	}
	if p, err := NewRegistry().New(Config{Provider: "stub"}); err != nil || p == nil || p.Name() != "stub" {
		t.Fatalf("New(stub) = %v, %v; want a stub", p, err)
	}
}
