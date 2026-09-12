// Package db menyediakan akses SQLite untuk autoclawpi.
// Schema: accounts + checkin_log + config.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var DB *sql.DB

// Account menyimpan satu akun AutoClaw.
type Account struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	AccessToken  string `json:"-"` // plaintext di memory, encrypted di disk
	RefreshToken string `json:"-"`
	Provider     string `json:"provider"` // zai | google
	UserID       string `json:"user_id"`
	UserName     string `json:"user_name"`
	Email        string `json:"email"` // di-extract dari JWT access token
	DeviceID     string `json:"device_id"`
	Points       int    `json:"points"`
	Active       bool   `json:"active"`
	CreatedAt    string `json:"created_at"`
	LastUsedAt   string `json:"last_used_at"`
}

// CheckinLog mencatat history check-in.
type CheckinLog struct {
	ID         int64  `json:"id"`
	AccountID  int64  `json:"account_id"`
	Date       string `json:"date"`       // YYYY-MM-DD
	TaskID     string `json:"task_id"`    // daily_signin, dll
	Points     int    `json:"points"`
	Status     string `json:"status"`     // success | already_done | failed
	DeviceID   string `json:"device_id"`
	CreatedAt  string `json:"created_at"`
}

// Init membuka/membuat database dan menjalankan migrasi.
func Init(dir string) error {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".autoclawpi")
	}
	_ = os.MkdirAll(dir, 0o700)
	dbPath := filepath.Join(dir, "autoclawpi.db")

	var err error
	DB, err = sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	DB.SetMaxOpenConns(1) // SQLite single-writer

	if err := migrate(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Close menutup database.
func Close() {
	if DB != nil {
		DB.Close()
	}
}

func migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS accounts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL DEFAULT '',
		access_token TEXT NOT NULL DEFAULT '',
		refresh_token TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT 'zai',
		user_id TEXT NOT NULL DEFAULT '',
		user_name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '',
		device_id TEXT NOT NULL DEFAULT '',
		points INTEGER NOT NULL DEFAULT 0,
		active INTEGER NOT NULL DEFAULT 1,
		created_at TEXT NOT NULL,
		last_used_at TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS checkin_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL,
		date TEXT NOT NULL,
		task_id TEXT NOT NULL,
		points INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'failed',
		device_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		FOREIGN KEY (account_id) REFERENCES accounts(id)
	);

	CREATE INDEX IF NOT EXISTS idx_checkin_log_account_date ON checkin_log(account_id, date);

	CREATE TABLE IF NOT EXISTS config (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		account_id INTEGER NOT NULL DEFAULT 0,
		model TEXT NOT NULL DEFAULT '',
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		cost REAL NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'success',
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (datetime('now'))
	);
	`
	_, err := DB.Exec(schema)
	if err != nil {
		return err
	}

	// Migrasi database lama: tambah kolom email jika belum ada.
	// Error "duplicate column name" diabaikan (kolom sudah ada).
	_, _ = DB.Exec(`ALTER TABLE accounts ADD COLUMN email TEXT NOT NULL DEFAULT ''`)
	return nil
}

// --- Account CRUD ---

// AddAccount menyimpan akun baru.
func AddAccount(name, accessToken, refreshToken, provider, userID, userName, deviceID, email string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := DB.Exec(`INSERT INTO accounts (name, access_token, refresh_token, provider, user_id, user_name, email, device_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, accessToken, refreshToken, provider, userID, userName, email, deviceID, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListAccounts mengembalikan semua akun.
func ListAccounts() ([]Account, error) {
	rows, err := DB.Query(`SELECT id, name, access_token, refresh_token, provider, user_id, user_name, email, device_id, points, active, created_at, last_used_at FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.Name, &a.AccessToken, &a.RefreshToken, &a.Provider, &a.UserID, &a.UserName, &a.Email, &a.DeviceID, &a.Points, &a.Active, &a.CreatedAt, &a.LastUsedAt); err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, nil
}

// GetAccount mengambil satu akun berdasarkan ID.
func GetAccount(id int64) (*Account, error) {
	row := DB.QueryRow(`SELECT id, name, access_token, refresh_token, provider, user_id, user_name, email, device_id, points, active, created_at, last_used_at FROM accounts WHERE id = ?`, id)
	var a Account
	if err := row.Scan(&a.ID, &a.Name, &a.AccessToken, &a.RefreshToken, &a.Provider, &a.UserID, &a.UserName, &a.Email, &a.DeviceID, &a.Points, &a.Active, &a.CreatedAt, &a.LastUsedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// GetActiveAccount mengembalikan akun aktif pertama (untuk single-account mode).
func GetActiveAccount() (*Account, error) {
	row := DB.QueryRow(`SELECT id, name, access_token, refresh_token, provider, user_id, user_name, email, device_id, points, active, created_at, last_used_at FROM accounts WHERE active = 1 ORDER BY id LIMIT 1`)
	var a Account
	if err := row.Scan(&a.ID, &a.Name, &a.AccessToken, &a.RefreshToken, &a.Provider, &a.UserID, &a.UserName, &a.Email, &a.DeviceID, &a.Points, &a.Active, &a.CreatedAt, &a.LastUsedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// UpdateAccount memperbarui data akun.
func UpdateAccount(a *Account) error {
	_, err := DB.Exec(`UPDATE accounts SET name=?, access_token=?, refresh_token=?, provider=?, user_id=?, user_name=?, email=?, device_id=?, points=?, active=?, last_used_at=? WHERE id=?`,
		a.Name, a.AccessToken, a.RefreshToken, a.Provider, a.UserID, a.UserName, a.Email, a.DeviceID, a.Points, a.Active, a.LastUsedAt, a.ID)
	return err
}

// UpdateAccountUsed mencatat waktu terakhir akun dipakai (untuk strategi least-used).
func UpdateAccountUsed(id int64) {
	_, _ = DB.Exec(`UPDATE accounts SET last_used_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), id)
}

// ValidStrategy melaporkan apakah nama strategi valid.
func ValidStrategy(s string) bool {
	return s == "round-robin" || s == "least-used" || s == "first"
}

// GetStrategy membaca strategi account selection tersimpan. Default "round-robin".
func GetStrategy() string {
	v, _ := GetConfig("strategy")
	if ValidStrategy(v) {
		return v
	}
	return "round-robin"
}

// SetStrategy menyimpan strategi account selection.
func SetStrategy(s string) error {
	if !ValidStrategy(s) {
		return fmt.Errorf("strategi tidak valid: %s", s)
	}
	return SetConfig("strategy", s)
}

// DeleteAccount menghapus akun.
func DeleteAccount(id int64) error {
	_, err := DB.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	return err
}

// UpdatePoints menambah poin akun.
func UpdatePoints(id int64, points int) error {
	_, err := DB.Exec(`UPDATE accounts SET points = ? WHERE id = ?`, points, id)
	return err
}

// --- Checkin Log ---

// AddCheckinLog mencatat hasil check-in.
func AddCheckinLog(accountID int64, date, taskID string, points int, status, deviceID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := DB.Exec(`INSERT INTO checkin_log (account_id, date, task_id, points, status, device_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		accountID, date, taskID, points, status, deviceID, now)
	return err
}

// GetCheckinLog mengembalikan log check-in untuk akun pada tanggal tertentu.
func GetCheckinLog(accountID int64, date, taskID string) (*CheckinLog, error) {
	row := DB.QueryRow(`SELECT id, account_id, date, task_id, points, status, device_id, created_at FROM checkin_log WHERE account_id = ? AND date = ? AND task_id = ?`, accountID, date, taskID)
	var l CheckinLog
	if err := row.Scan(&l.ID, &l.AccountID, &l.Date, &l.TaskID, &l.Points, &l.Status, &l.DeviceID, &l.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &l, nil
}

// ListCheckinLog mengembalikan semua log check-in untuk akun.
func ListCheckinLog(accountID int64, limit int) ([]CheckinLog, error) {
	rows, err := DB.Query(`SELECT id, account_id, date, task_id, points, status, device_id, created_at FROM checkin_log WHERE account_id = ? ORDER BY created_at DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []CheckinLog
	for rows.Next() {
		var l CheckinLog
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Date, &l.TaskID, &l.Points, &l.Status, &l.DeviceID, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// --- Config ---

// GetConfig mengambil nilai konfigurasi.
func GetConfig(key string) (string, error) {
	row := DB.QueryRow(`SELECT value FROM config WHERE key = ?`, key)
	var val string
	if err := row.Scan(&val); err != nil {
		if err == sql.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	return val, nil
}

// SetConfig menyimpan nilai konfigurasi.
func SetConfig(key, value string) error {
	_, err := DB.Exec(`INSERT OR REPLACE INTO config (key, value) VALUES (?, ?)`, key, value)
	return err
}

// ── Logs ────────────────────────────────────────────────────────────

type LogEntry struct {
	ID               int64   `json:"id"`
	AccountID        int64   `json:"account_id"`
	AccountName      string  `json:"account_name"` // di-join dari tabel accounts
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	TotalTokens      int     `json:"total_tokens"`
	Cost             float64 `json:"cost"`
	Status           string  `json:"status"`
	Error            string  `json:"error"`
	CreatedAt        string  `json:"created_at"`
}

func AddLog(l *LogEntry) (int64, error) {
	res, err := DB.Exec(`INSERT INTO logs (account_id, model, prompt_tokens, completion_tokens, total_tokens, cost, status, error, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))`,
		l.AccountID, l.Model, l.PromptTokens, l.CompletionTokens, l.TotalTokens, l.Cost, l.Status, l.Error)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// logSinceClause mengubah parameter range (today|7d|30d|60d) menjadi klausa SQL.
// String kosong berarti tanpa filter (semua waktu).
func logSinceClause(since string) string {
	switch since {
	case "today":
		return "created_at >= datetime('now', 'start of day')"
	case "7d":
		return "created_at >= datetime('now', '-7 days')"
	case "30d":
		return "created_at >= datetime('now', '-30 days')"
	case "60d":
		return "created_at >= datetime('now', '-60 days')"
	default:
		return ""
	}
}

func ListLogs(limit int, since string) ([]LogEntry, error) {
	if limit < 1 {
		limit = 50
	}
	query := `SELECT l.id, l.account_id, COALESCE(NULLIF(a.email, ''), NULLIF(a.name, ''), NULLIF(a.user_name, ''), ''),
	l.model, l.prompt_tokens, l.completion_tokens, l.total_tokens, l.cost, l.status, l.error, l.created_at
	FROM logs l LEFT JOIN accounts a ON a.id = l.account_id`
	if clause := logSinceClause(since); clause != "" {
		query += " WHERE l." + clause
	}
	query += " ORDER BY l.id DESC LIMIT ?"
	rows, err := DB.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []LogEntry
	for rows.Next() {
		var l LogEntry
		if err := rows.Scan(&l.ID, &l.AccountID, &l.AccountName, &l.Model, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.Cost, &l.Status, &l.Error, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

func LogStats(since string) (totalReq int, totalTokens int, totalCost float64, err error) {
	query := `SELECT COUNT(*), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost),0) FROM logs`
	if clause := logSinceClause(since); clause != "" {
		query += " WHERE " + clause
	}
	err = DB.QueryRow(query).Scan(&totalReq, &totalTokens, &totalCost)
	return
}

// ClearLogs menghapus log request sesuai filter periode (today|7d|30d|60d|all).
// Mengembalikan jumlah baris yang terhapus.
func ClearLogs(since string) (int64, error) {
	query := `DELETE FROM logs`
	if clause := logSinceClause(since); clause != "" {
		query += " WHERE " + clause
	}
	res, err := DB.Exec(query)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}