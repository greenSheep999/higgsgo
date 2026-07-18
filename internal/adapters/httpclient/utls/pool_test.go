package utls

import (
	"context"
	"testing"

	"github.com/greensheep999/higgsgo/internal/domain"
)

// TestPool_ResolveByBoundProxy verifies the two cheap invariants the
// pool must uphold: (1) empty bound_proxy_url falls through to the
// default (returns nil so upstream.Client uses its own default field),
// and (2) each distinct URL builds and caches its own *Client.
func TestPool_ResolveByBoundProxy(t *testing.T) {
	def, err := New(Config{})
	if err != nil {
		t.Fatalf("default client: %v", err)
	}
	p := NewPool(Config{}, def)

	// Empty bound URL → fallback signal (nil, nil).
	got, err := p.Resolve(context.Background(), &domain.Account{ID: "a"})
	if err != nil {
		t.Fatalf("resolve empty: %v", err)
	}
	if got != nil {
		t.Errorf("empty URL should return nil to fall back; got %v", got)
	}

	// Two distinct URLs → two distinct clients.
	c1, err := p.Resolve(context.Background(), &domain.Account{
		ID: "a", BoundProxyURL: "socks5://user:pass@10.0.0.1:1080",
	})
	if err != nil {
		t.Fatalf("resolve socks5://a: %v", err)
	}
	c2, err := p.Resolve(context.Background(), &domain.Account{
		ID: "b", BoundProxyURL: "socks5://user:pass@10.0.0.2:1080",
	})
	if err != nil {
		t.Fatalf("resolve socks5://b: %v", err)
	}
	if c1 == c2 {
		t.Errorf("distinct URLs must resolve to distinct clients")
	}

	// Same URL twice → cached same client.
	c3, err := p.Resolve(context.Background(), &domain.Account{
		ID: "a2", BoundProxyURL: "socks5://user:pass@10.0.0.1:1080",
	})
	if err != nil {
		t.Fatalf("resolve socks5://a again: %v", err)
	}
	if c1 != c3 {
		t.Errorf("repeat URL must return cached client")
	}
}

// TestPool_MalformedURLReturnsErrorAndCaches makes sure a bad URL
// doesn't get retried on every request — the resolver returns the
// build error, upstream.Client logs it and falls back to the default.
func TestPool_MalformedURLReturnsErrorAndCaches(t *testing.T) {
	def, _ := New(Config{})
	p := NewPool(Config{}, def)

	acc := &domain.Account{ID: "bad", BoundProxyURL: "://not-a-url"}
	first, err := p.Resolve(context.Background(), acc)
	if err == nil {
		t.Errorf("malformed URL should return an error")
	}
	if first != nil {
		t.Errorf("malformed URL should return nil client")
	}

	// Second call must return the same cached error, not attempt to
	// rebuild — protects against a flood of build attempts for a
	// broken proxy.
	second, err2 := p.Resolve(context.Background(), acc)
	if err2 == nil {
		t.Errorf("cached error should still be returned")
	}
	if err.Error() != err2.Error() {
		t.Errorf("cached error mismatch: first=%v second=%v", err, err2)
	}
	if second != nil {
		t.Errorf("cached error path should still return nil client")
	}

	// Invalidate clears the cache; next call rebuilds (still fails,
	// but re-attempts).
	p.Invalidate("://not-a-url")
	if _, ok := p.buildErrs["://not-a-url"]; ok {
		t.Errorf("Invalidate should clear buildErrs")
	}
}

// TestPool_NilAccount is a defensive check — a nil account (should
// never happen at runtime, but we guard it) must not panic.
func TestPool_NilAccount(t *testing.T) {
	def, _ := New(Config{})
	p := NewPool(Config{}, def)

	got, err := p.Resolve(context.Background(), nil)
	if err != nil {
		t.Errorf("nil account: err=%v want nil", err)
	}
	if got != nil {
		t.Errorf("nil account: client=%v want nil", got)
	}
}
