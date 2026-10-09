// SPDX-License-Identifier: GPL-3.0-or-later

// Package store persists devices, API keys, and pending pairings in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("store: not found")

// migrations are applied in order; the index of the last applied one is kept
// in SQLite's user_version. Append only — never edit a shipped migration.
var migrations = []string{
	`CREATE TABLE devices (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		token_hash   TEXT NOT NULL UNIQUE,
		seal_key     BLOB NOT NULL,
		fcm_token    TEXT NOT NULL DEFAULT '',
		created_at   INTEGER NOT NULL,
		last_seen_at INTEGER NOT NULL
	);
	CREATE TABLE api_keys (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		key_hash     TEXT NOT NULL UNIQUE,
		created_at   INTEGER NOT NULL,
		last_used_at INTEGER
	);
	CREATE TABLE pairings (
		code_hash  TEXT PRIMARY KEY,
		expires_at INTEGER NOT NULL
	);`,
}

type Device struct {
	ID         string
	Name       string
	SealKey    []byte
	FCMToken   string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

type APIKey struct {
	ID         string
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; one connection avoids SQLITE_BUSY churn
	// and keeps in-memory databases (used by tests) coherent.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CreatePairing(ctx context.Context, codeHash string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO pairings (code_hash, expires_at) VALUES (?, ?)", codeHash, expiresAt.Unix())
	return err
}

// RedeemPairing consumes a pairing code and creates the device it authorizes,
// atomically: a code can be redeemed at most once, and only before it expires.
func (s *Store) RedeemPairing(ctx context.Context, codeHash string, now time.Time, d Device, tokenHash string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, "DELETE FROM pairings WHERE expires_at <= ?", now.Unix()); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM pairings WHERE code_hash = ?", codeHash)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if err := tx.Commit(); err != nil {
			return err
		}
		return ErrNotFound
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, seal_key, fcm_token, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Name, tokenHash, d.SealKey, d.FCMToken, now.Unix(), now.Unix())
	if err != nil {
		return err
	}
	return tx.Commit()
}

const deviceColumns = "id, name, seal_key, fcm_token, created_at, last_seen_at"

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var created, seen int64
	if err := row.Scan(&d.ID, &d.Name, &d.SealKey, &d.FCMToken, &created, &seen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Device{}, ErrNotFound
		}
		return Device{}, err
	}
	d.CreatedAt = time.Unix(created, 0).UTC()
	d.LastSeenAt = time.Unix(seen, 0).UTC()
	return d, nil
}

func (s *Store) DeviceByTokenHash(ctx context.Context, tokenHash string, now time.Time) (Device, error) {
	d, err := scanDevice(s.db.QueryRowContext(ctx,
		"SELECT "+deviceColumns+" FROM devices WHERE token_hash = ?", tokenHash))
	if err != nil {
		return Device{}, err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE devices SET last_seen_at = ? WHERE id = ?", now.Unix(), d.ID)
	return d, err
}

func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+deviceColumns+" FROM devices ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var devices []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (s *Store) SetFCMToken(ctx context.Context, deviceID, fcmToken string) error {
	return s.execOne(ctx, "UPDATE devices SET fcm_token = ? WHERE id = ?", fcmToken, deviceID)
}

func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	return s.execOne(ctx, "DELETE FROM devices WHERE id = ?", id)
}

func (s *Store) CreateAPIKey(ctx context.Context, k APIKey, keyHash string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO api_keys (id, name, key_hash, created_at) VALUES (?, ?, ?, ?)",
		k.ID, k.Name, keyHash, k.CreatedAt.Unix())
	return err
}

func (s *Store) APIKeyByHash(ctx context.Context, keyHash string, now time.Time) (APIKey, error) {
	k, err := scanAPIKey(s.db.QueryRowContext(ctx,
		"SELECT id, name, created_at, last_used_at FROM api_keys WHERE key_hash = ?", keyHash))
	if err != nil {
		return APIKey{}, err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE api_keys SET last_used_at = ? WHERE id = ?", now.Unix(), k.ID)
	return k, err
}

func (s *Store) APIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, created_at, last_used_at FROM api_keys ORDER BY created_at")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Store) DeleteAPIKey(ctx context.Context, id string) error {
	return s.execOne(ctx, "DELETE FROM api_keys WHERE id = ?", id)
}

func scanAPIKey(row interface{ Scan(...any) error }) (APIKey, error) {
	var k APIKey
	var created int64
	var used sql.NullInt64
	if err := row.Scan(&k.ID, &k.Name, &created, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return APIKey{}, ErrNotFound
		}
		return APIKey{}, err
	}
	k.CreatedAt = time.Unix(created, 0).UTC()
	if used.Valid {
		t := time.Unix(used.Int64, 0).UTC()
		k.LastUsedAt = &t
	}
	return k, nil
}

func (s *Store) execOne(ctx context.Context, query string, args ...any) error {
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
