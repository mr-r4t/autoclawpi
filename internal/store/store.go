// Package store menyediakan akses kredensial akun (single-account mode).
// Backend: SQLite via internal/db.
package store

import (
	"fmt"
	"os"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/client"
	"github.com/hirotomasato/autoclawpi/internal/db"
)

// Creds adalah kredensial akun AutoClaw (single-account compatibility).
type Creds struct {
	AccessToken  string `json:"access"`
	RefreshToken string `json:"refresh,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	UserName     string `json:"user_name,omitempty"`
	Email        string `json:"email,omitempty"`
	Provider     string `json:"provider,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	SavedAt      string `json:"saved_at,omitempty"`
}

// ErrNotFound berarti belum ada akun.
var ErrNotFound = fmt.Errorf("belum login (tidak ada kredensial tersimpan)")

// Save menyimpan atau memperbarui akun pertama (single-account).
func Save(c *Creds) error {
	existing, err := db.GetActiveAccount()
	if err != nil {
		return err
	}
	if c.DeviceID == "" {
		h, _ := os.Hostname()
		if h == "" {
			h = "unknown"
		}
		c.DeviceID = h + "-autoclawpi"
	}
	// Auto-extract email dari JWT access token jika belum ada
	if c.Email == "" && c.AccessToken != "" {
		if email, jwtUserID := client.DecodeJWT(c.AccessToken); email != "" {
			c.Email = email
			if c.UserID == "" && jwtUserID != "" {
				c.UserID = jwtUserID
			}
		}
	}
	if existing != nil {
		// update existing
		existing.AccessToken = c.AccessToken
		existing.RefreshToken = c.RefreshToken
		existing.Provider = c.Provider
		existing.UserID = c.UserID
		existing.UserName = c.UserName
		existing.Email = c.Email
		existing.DeviceID = c.DeviceID
		existing.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		return db.UpdateAccount(existing)
	}
	// create new
	name := c.UserName
	if name == "" && c.Email != "" {
		name = c.Email
	}
	_, err = db.AddAccount(name, c.AccessToken, c.RefreshToken, c.Provider, c.UserID, c.UserName, c.DeviceID, c.Email)
	return err
}

// Load membaca kredensial akun pertama.
func Load() (*Creds, error) {
	a, err := db.GetActiveAccount()
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	return &Creds{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		UserID:       a.UserID,
		UserName:     a.UserName,
		Email:        a.Email,
		Provider:     a.Provider,
		DeviceID:     a.DeviceID,
		SavedAt:      a.CreatedAt,
	}, nil
}

// Clear menghapus akun pertama.
func Clear() error {
	existing, err := db.GetActiveAccount()
	if err != nil {
		return err
	}
	if existing != nil {
		return db.DeleteAccount(existing.ID)
	}
	return nil
}