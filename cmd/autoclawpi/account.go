package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hirotomasato/autoclawpi/internal/client"
	"github.com/hirotomasato/autoclawpi/internal/db"
)

func cmdAccount(args []string) error {
	if len(args) == 0 {
		return listAccounts()
	}
	switch args[0] {
	case "list", "ls":
		return listAccounts()
	case "add":
		return addAccount(args[1:])
	case "remove", "rm", "delete":
		return removeAccount(args[1:])
	case "show":
		return showAccount(args[1:])
	case "refresh":
		return refreshAccounts(args[1:])
	default:
		return fmt.Errorf("subcommand: list | add | remove | show | refresh")
	}
}

func listAccounts() error {
	accounts, err := db.ListAccounts()
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		fmt.Println("tidak ada akun. jalankan 'autoclawpi login' atau 'autoclawpi import'")
		return nil
	}
	fmt.Printf("%-4s %-20s %-8s %-12s %-8s %s\n", "ID", "Name", "Provider", "User", "Points", "Last Used")
	for _, a := range accounts {
		active := "✓"
		if !a.Active {
			active = "✗"
		}
		fmt.Printf("%-4d %-20s %-8s %-12s %-8d %s %s\n", a.ID, truncate(a.Name, 18), a.Provider, truncate(a.UserName, 10), a.Points, active, truncate(a.LastUsedAt, 16))
	}
	return nil
}

func addAccount(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	name := fs.String("name", "", "nama akun")
	access := fs.String("access", "", "access token")
	refresh := fs.String("refresh", "", "refresh token (opsional)")
	provider := fs.String("provider", "zai", "zai | google")
	fs.Parse(args)

	if *access == "" {
		fmt.Fprintf(os.Stderr, "usage: autoclawpi account add --access <token> [--refresh <token>] [--name <name>] [--provider zai|google]\n")
		os.Exit(2)
	}
	deviceID := fmt.Sprintf("autoclawpi-%d", time.Now().UnixNano())
	// Extract email dari JWT access token
	email, jwtUserID := client.DecodeJWT(*access)
	if *name == "" && email != "" {
		*name = email
	}
	id, err := db.AddAccount(*name, *access, *refresh, *provider, jwtUserID, "", deviceID, email)
	if err != nil {
		return err
	}
	fmt.Printf("akun #%d ditambahkan\n", id)
	return nil
}

func removeAccount(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: autoclawpi account remove <id>")
	}
	var id int64
	if _, err := fmt.Sscanf(args[0], "%d", &id); err != nil {
		return fmt.Errorf("id harus angka")
	}
	if err := db.DeleteAccount(id); err != nil {
		return err
	}
	fmt.Printf("akun #%d dihapus\n", id)
	return nil
}

func showAccount(args []string) error {
	var id int64
	if len(args) > 0 {
		fmt.Sscanf(args[0], "%d", &id)
	}
	var a *db.Account
	var err error
	if id > 0 {
		a, err = db.GetAccount(id)
	} else {
		a, err = db.GetActiveAccount()
	}
	if err != nil {
		return err
	}
	if a == nil {
		return fmt.Errorf("akun tidak ditemukan")
	}

	// fetch saldo real-time dari server
	balance, _ := fetchBalance(a.AccessToken)
	if balance > 0 {
		db.UpdatePoints(a.ID, balance) // update total
		a.Points = balance             // refresh local struct
	}

	fmt.Printf("ID        : %d\n", a.ID)
	fmt.Printf("Name      : %s\n", a.Name)
	fmt.Printf("Provider  : %s\n", a.Provider)
	fmt.Printf("UserID    : %s\n", a.UserID)
	fmt.Printf("UserName  : %s\n", a.UserName)
	fmt.Printf("Email     : %s\n", a.Email)
	fmt.Printf("DeviceID  : %s\n", a.DeviceID)
	fmt.Printf("Points    : %d pts (server: %d)\n", a.Points, balance)
	fmt.Printf("Active    : %v\n", a.Active)
	fmt.Printf("Created   : %s\n", a.CreatedAt)
	fmt.Printf("LastUsed  : %s\n", a.LastUsedAt)
	fmt.Printf("Access    : %s... (len=%d)\n", redactToken(a.AccessToken), len(a.AccessToken))
	fmt.Printf("Refresh   : %s... (len=%d)\n", redactToken(a.RefreshToken), len(a.RefreshToken))
	return nil
}

// refreshAccounts me-refresh token satu akun (account refresh <id>) atau
// semua akun (account refresh / account refresh all).
func refreshAccounts(args []string) error {
	_, cl := loadAll()
	var targets []*db.Account

	if len(args) == 0 || (len(args) == 1 && args[0] == "all") {
		accounts, err := db.ListAccounts()
		if err != nil {
			return err
		}
		if len(accounts) == 0 {
			fmt.Println("tidak ada akun.")
			return nil
		}
		for i := range accounts {
			targets = append(targets, &accounts[i])
		}
	} else {
		for _, arg := range args {
			var id int64
			if _, err := fmt.Sscanf(arg, "%d", &id); err != nil || id <= 0 {
				return fmt.Errorf("id tidak valid: %s", arg)
			}
			a, err := db.GetAccount(id)
			if err != nil {
				return err
			}
			if a == nil {
				return fmt.Errorf("akun #%d tidak ditemukan", id)
			}
			targets = append(targets, a)
		}
	}

	failed := 0
	for _, a := range targets {
		if a.RefreshToken == "" {
			fmt.Printf("✗ #%d %-20s tidak ada refresh token\n", a.ID, a.Name)
			failed++
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := cl.Refresh(ctx, a.RefreshToken)
		cancel()
		if err != nil || out == nil || out.Code != 0 || out.Data == nil || out.Data.AccessToken == "" {
			msg := "unknown"
			if err != nil {
				msg = err.Error()
			} else if out != nil {
				msg = out.Msg
			}
			fmt.Printf("✗ #%d %-20s refresh gagal: %s\n", a.ID, a.Name, msg)
			failed++
			continue
		}
		a.AccessToken = out.Data.AccessToken
		if out.Data.RefreshToken != "" {
			a.RefreshToken = out.Data.RefreshToken
		}
		// Backfill email dari JWT jika akun belum punya
		if a.Email == "" {
			if email, _ := client.DecodeJWT(a.AccessToken); email != "" {
				a.Email = email
				if a.Name == "" {
					a.Name = email
				}
			}
		}
		if err := db.UpdateAccount(a); err != nil {
			fmt.Printf("✗ #%d %-20s simpan gagal: %v\n", a.ID, a.Name, err)
			failed++
			continue
		}
		fmt.Printf("✓ #%d %-20s token diperbarui\n", a.ID, a.Name)
	}

	if failed > 0 {
		return fmt.Errorf("%d/%d akun gagal di-refresh", failed, len(targets))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func redactToken(s string) string {
	if s == "" {
		return "(kosong)"
	}
	if len(s) > 20 {
		return s[:20]
	}
	return s
}