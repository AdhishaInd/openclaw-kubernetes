package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// randomToken returns a hex-encoded random token for per-user gateway auth.
func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensureUser creates all per-user resources (idempotent). The Deployment starts
// scaled to zero; the activating proxy wakes it on first request.
func (s *Server) ensureUser(ctx context.Context, id, email, passwordHash string) error {
	token, err := randomToken()
	if err != nil {
		return err
	}
	return s.k8s.createUserResources(ctx, id, email, passwordHash, token)
}
