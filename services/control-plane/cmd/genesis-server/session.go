package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/genesis-ai-factory/control-plane/internal/usecase"
)

// localSession is the handshake artifact between the server and the local
// desktop app or CLI on a single-user installation.
type localSession struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
	Email        string `json:"email"`
	UserID       string `json:"user_id"`
	APIBase      string `json:"api_base"`
}

func localSessionPath(dataDir string) string {
	return filepath.Join(dataDir, "session.json")
}

// writeLocalSession persists credentials for local clients.
//
// The file is written with owner-only permissions: it contains a live refresh
// token, so anything broader would hand every local account a session.
func writeLocalSession(dataDir string, s *usecase.Session) error {
	payload := localSession{
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		ExpiresAt:    s.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
		Email:        s.User.Email,
		UserID:       s.User.ID.String(),
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}

	path := localSessionPath(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write session file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("finalise session file: %w", err)
	}
	return nil
}
