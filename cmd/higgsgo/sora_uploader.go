package main

import (
	"bytes"
	"context"
	"fmt"

	"github.com/greensheep999/higgsgo/internal/core/upstream"
	"github.com/greensheep999/higgsgo/internal/domain"
	"github.com/greensheep999/higgsgo/internal/ports"
)

// soraUploader adapts upstream.Client.UploadImage to the SoraMediaUploader
// port on v1.Handler. It borrows any active higgsfield account to sign the
// upload — media is account-agnostic once committed (the returned media_id
// is usable from any job on any account), so no need to reserve a slot
// via the pool router. Falls back to the first active account it finds.
type soraUploader struct {
	client   *upstream.Client
	accounts ports.AccountStore
}

func newSoraUploader(client *upstream.Client, accounts ports.AccountStore) *soraUploader {
	return &soraUploader{client: client, accounts: accounts}
}

// UploadImage satisfies v1.SoraMediaUploader. contentType is a MIME type
// like "image/jpeg"; body is the raw file bytes.
func (u *soraUploader) UploadImage(ctx context.Context, contentType string, body []byte) (string, error) {
	acct, err := u.pickActive(ctx)
	if err != nil {
		return "", err
	}
	return u.client.UploadImage(ctx, acct, contentType, bytes.NewReader(body))
}

// pickActive returns any active account. Media upload does not consume
// the account's in-flight slot and does not care which pool tier the
// account belongs to.
func (u *soraUploader) pickActive(ctx context.Context) (*domain.Account, error) {
	list, err := u.accounts.List(ctx, ports.AccountFilter{Status: domain.StatusActive})
	if err != nil {
		return nil, fmt.Errorf("list active accounts: %w", err)
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("no active higgsfield account available for media upload")
	}
	return &list[0], nil
}
