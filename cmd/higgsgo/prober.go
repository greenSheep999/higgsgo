package main

// upstreamProber adapts *upstream.Client to admin.Prober. The admin
// package deliberately avoids importing internal/core/upstream so its
// unit tests can stub Prober directly — see internal/api/admin/
// accounts_probe.go for the local ProbeWallet type this converts to.
// See docs/ROADMAP.md P2-6.

import (
	"context"

	"github.com/greensheep999/higgsgo/internal/api/admin"
	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
)

type upstreamProber struct {
	c *upstream.Client
}

// FetchWallet delegates to the upstream client and translates its
// Wallet struct into the shape the admin handler surfaces. The
// translation is trivial — the interesting work is that this call
// goes through the same JWT minter, per-account HTTP client (P1-5),
// and TLS fingerprint the pool uses, so a green probe is a strong
// signal.
func (p *upstreamProber) FetchWallet(ctx context.Context, account *domain.Account) (*admin.ProbeWallet, error) {
	w, err := p.c.FetchWallet(ctx, account)
	if err != nil {
		return nil, err
	}
	return &admin.ProbeWallet{
		WorkspaceID:         w.WorkspaceID,
		SubscriptionBalance: w.SubscriptionBalance,
		CreditsBalance:      w.CreditsBalance,
	}, nil
}
