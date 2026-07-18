package cpaplugin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/greensheep999/higgsgo/internal/api/cpaplugin"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// TestStubRegistrar asserts every method returns ErrRegistrarDisabled.
// The admin handler in this package leans on this to answer 503, and the
// registrar_admin_test suite mirrors the check on the HTTP surface.
func TestStubRegistrar(t *testing.T) {
	t.Parallel()
	var r ports.Registrar = cpaplugin.StubRegistrar{}
	ctx := context.Background()

	if _, err := r.Enqueue(ctx, ports.RegistrationRequest{Email: "a@b.co"}); !errors.Is(err, domain.ErrRegistrarDisabled) {
		t.Fatalf("Enqueue: want ErrRegistrarDisabled, got %v", err)
	}
	if _, err := r.GetStatus(ctx, "reg_x"); !errors.Is(err, domain.ErrRegistrarDisabled) {
		t.Fatalf("GetStatus: want ErrRegistrarDisabled, got %v", err)
	}
	if _, err := r.List(ctx, ports.RegistrationFilter{}); !errors.Is(err, domain.ErrRegistrarDisabled) {
		t.Fatalf("List: want ErrRegistrarDisabled, got %v", err)
	}
	if err := r.Retry(ctx, "reg_x"); !errors.Is(err, domain.ErrRegistrarDisabled) {
		t.Fatalf("Retry: want ErrRegistrarDisabled, got %v", err)
	}
}
