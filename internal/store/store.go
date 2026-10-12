// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store keeps devices, pairing codes and book records in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/vagramchic/bookmarks-server/internal/pairing"
)

// DBFile is the database file name inside the data directory.
const DBFile = "bookmarks.db"

// MaxFailedAttempts is how many wrong proofs burn a pairing code.
const MaxFailedAttempts = 10

var (
	ErrNotFound      = errors.New("not found")
	ErrInvalidCode   = errors.New("invalid pairing code")
	ErrAlreadyPaired = errors.New("already paired")
	ErrDeviceLimit   = errors.New("device limit reached")
)

// Store is a handle to the database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*Store, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrations[i] brings the schema from version i to i+1.
var migrations = []string{
	`CREATE TABLE devices (
		id           TEXT PRIMARY KEY,
		name         TEXT NOT NULL,
		platform     TEXT NOT NULL,
		fingerprint  TEXT NOT NULL UNIQUE,
		paired_at    INTEGER NOT NULL,
		last_seen_at INTEGER
	);
	CREATE TABLE pairing_codes (
		code            TEXT NOT NULL,
		expires_at      INTEGER NOT NULL,
		failed_attempts INTEGER NOT NULL DEFAULT 0
	);
	CREATE TABLE records (
		book_id    TEXT PRIMARY KEY,
		file_name  TEXT NOT NULL,
		file_size  INTEGER NOT NULL,
		title      TEXT,
		last_page  INTEGER NOT NULL DEFAULT 0,
		page_count INTEGER NOT NULL DEFAULT 0,
		progress   REAL NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL,
		deleted    INTEGER NOT NULL DEFAULT 0,
		has_file   INTEGER NOT NULL DEFAULT 0,
		updated_by TEXT NOT NULL,
		rev        INTEGER NOT NULL UNIQUE
	);
	INSERT INTO meta(key, value) VALUES ('rev', '0');`,
}

// SchemaVersion is the version this binary migrates to.
var SchemaVersion = len(migrations)

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	version := 0
	var v string
	switch err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		if version, err = strconv.Atoi(v); err != nil {
			return fmt.Errorf("schema_version %q: %w", v, err)
		}
	}
	if version > len(migrations) {
		return fmt.Errorf("база создана более новой версией сервера (схема %d, поддерживается %d)", version, len(migrations))
	}
	for ; version < len(migrations); version++ {
		if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
			return fmt.Errorf("migration %d: %w", version+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(version)); err != nil {
		return err
	}
	return tx.Commit()
}

// Version returns the schema version recorded in the database.
func (s *Store) Version(ctx context.Context) (int, error) {
	var v string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v); err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

func toMillis(t time.Time) int64 { return t.UnixMilli() }

// ----- devices -----

// Device is a paired device.
type Device struct {
	ID          string
	Name        string
	Platform    string
	Fingerprint string
	PairedAt    time.Time
	LastSeenAt  *time.Time
}

const deviceCols = `id, name, platform, fingerprint, paired_at, last_seen_at`

type scanner interface{ Scan(dest ...any) error }

func scanDevice(row scanner) (Device, error) {
	var d Device
	var paired int64
	var seen sql.NullInt64
	if err := row.Scan(&d.ID, &d.Name, &d.Platform, &d.Fingerprint, &paired, &seen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return d, ErrNotFound
		}
		return d, err
	}
	d.PairedAt = time.UnixMilli(paired).UTC()
	if seen.Valid {
		t := time.UnixMilli(seen.Int64).UTC()
		d.LastSeenAt = &t
	}
	return d, nil
}

// DeviceByFingerprint finds a device by its certificate fingerprint.
func (s *Store) DeviceByFingerprint(ctx context.Context, fp string) (Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE fingerprint = ?`, fp))
}

// Devices lists paired devices, oldest first.
func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY paired_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func countDevices(ctx context.Context, q querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices`).Scan(&n)
	return n, err
}

// CountDevices returns the number of paired devices.
func (s *Store) CountDevices(ctx context.Context) (int, error) {
	return countDevices(ctx, s.db)
}

// RemoveDevice forgets a device. Its records and files stay.
func (s *Store) RemoveDevice(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchDevice sets last_seen_at unless it was set less than a minute ago.
func (s *Store) TouchDevice(ctx context.Context, id string, now time.Time) error {
	ms := toMillis(now)
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen_at = ?
		WHERE id = ? AND (last_seen_at IS NULL OR last_seen_at <= ?)`, ms, id, ms-60_000)
	return err
}

// NewDeviceID returns 16 random bytes as unpadded lower-case base32.
func NewDeviceID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)), nil
}

// ----- pairing -----

// SetPairingCode stores a new code, replacing any previous one.
func (s *Store) SetPairingCode(ctx context.Context, code string, expiresAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pairing_codes(code, expires_at) VALUES (?, ?)`,
		pairing.Normalize(code), toMillis(expiresAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// PairRequest is what Pair needs to check a client's proof.
type PairRequest struct {
	Name              string
	Platform          string
	ServerFingerprint string
	ClientFingerprint string
	ClientProof       string
	Now               time.Time
	MaxDevices        int // 0 means no limit
}

// Pair checks the client's proof against the current code. On success it
// creates the device, burns the code and returns the device and the code
// (the HMAC key for the server proof). A wrong proof counts as a failed
// attempt; after MaxFailedAttempts the code is burnt. If MaxDevices devices
// are already paired, a correct proof gets ErrDeviceLimit and the code stays
// valid, so it can be used once a device is removed.
func (s *Store) Pair(ctx context.Context, req PairRequest) (Device, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, "", err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM devices WHERE fingerprint = ?`, req.ClientFingerprint).Scan(&exists); err != nil {
		return Device{}, "", err
	}
	if exists > 0 {
		return Device{}, "", ErrAlreadyPaired
	}

	var code string
	var expires int64
	var failed int
	err = tx.QueryRowContext(ctx, `SELECT code, expires_at, failed_attempts FROM pairing_codes LIMIT 1`).Scan(&code, &expires, &failed)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, "", ErrInvalidCode
	}
	if err != nil {
		return Device{}, "", err
	}
	if toMillis(req.Now) >= expires || failed >= MaxFailedAttempts {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes`); err != nil {
			return Device{}, "", err
		}
		if err := tx.Commit(); err != nil {
			return Device{}, "", err
		}
		return Device{}, "", ErrInvalidCode
	}
	want := pairing.ClientProof(code, req.ServerFingerprint, req.ClientFingerprint)
	if !pairing.Verify(req.ClientProof, want) {
		if failed+1 >= MaxFailedAttempts {
			_, err = tx.ExecContext(ctx, `DELETE FROM pairing_codes`)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE pairing_codes SET failed_attempts = failed_attempts + 1`)
		}
		if err != nil {
			return Device{}, "", err
		}
		if err := tx.Commit(); err != nil {
			return Device{}, "", err
		}
		return Device{}, "", ErrInvalidCode
	}

	if req.MaxDevices > 0 {
		n, err := countDevices(ctx, tx)
		if err != nil {
			return Device{}, "", err
		}
		if n >= req.MaxDevices {
			return Device{}, "", ErrDeviceLimit
		}
	}

	id, err := NewDeviceID()
	if err != nil {
		return Device{}, "", err
	}
	d := Device{
		ID:          id,
		Name:        req.Name,
		Platform:    req.Platform,
		Fingerprint: req.ClientFingerprint,
		PairedAt:    time.UnixMilli(toMillis(req.Now)).UTC(),
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO devices(id, name, platform, fingerprint, paired_at) VALUES (?, ?, ?, ?, ?)`,
		d.ID, d.Name, d.Platform, d.Fingerprint, toMillis(d.PairedAt)); err != nil {
		return Device{}, "", err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pairing_codes`); err != nil {
		return Device{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Device{}, "", err
	}
	return d, code, nil
}

// ----- records -----

// Record is a book record. JSON names follow api/openapi.yaml.
type Record struct {
	BookID    string  `json:"bookId"`
	FileName  string  `json:"fileName"`
	FileSize  int64   `json:"fileSize"`
	Title     *string `json:"title"`
	LastPage  int64   `json:"lastPage"`
	PageCount int64   `json:"pageCount"`
	Progress  float64 `json:"progress"`
	UpdatedAt int64   `json:"updatedAt"`
	Deleted   bool    `json:"deleted"`
	HasFile   bool    `json:"hasFile"`
	UpdatedBy string  `json:"updatedBy"`
	Rev       int64   `json:"rev"`
}

const recordCols = `book_id, file_name, file_size, title, last_page, page_count, progress, updated_at, deleted, has_file, updated_by, rev`

func scanRecord(row scanner) (Record, error) {
	var r Record
	var title sql.NullString
	err := row.Scan(&r.BookID, &r.FileName, &r.FileSize, &title, &r.LastPage, &r.PageCount,
		&r.Progress, &r.UpdatedAt, &r.Deleted, &r.HasFile, &r.UpdatedBy, &r.Rev)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if title.Valid {
		r.Title = &title.String
	}
	return r, err
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getRecord(ctx context.Context, q querier, bookID string) (Record, error) {
	return scanRecord(q.QueryRowContext(ctx, `SELECT `+recordCols+` FROM records WHERE book_id = ?`, bookID))
}

// Record returns the record for bookID.
func (s *Store) Record(ctx context.Context, bookID string) (Record, error) {
	return getRecord(ctx, s.db, bookID)
}

func nextRev(ctx context.Context, tx *sql.Tx) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx, `UPDATE meta SET value = CAST(value AS INTEGER) + 1 WHERE key = 'rev' RETURNING CAST(value AS INTEGER)`).Scan(&rev)
	return rev, err
}

// PushRecords stores each record that is new or has a strictly greater
// updatedAt than the stored one, all in one transaction, and returns what is
// stored afterwards, one per input in the same order. HasFile, UpdatedBy and
// Rev of the inputs are ignored.
func (s *Store) PushRecords(ctx context.Context, deviceID string, in []Record) ([]Record, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]Record, 0, len(in))
	for _, r := range in {
		cur, err := getRecord(ctx, tx, r.BookID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return nil, err
		case r.UpdatedAt <= cur.UpdatedAt:
			out = append(out, cur)
			continue
		}
		rev, err := nextRev(ctx, tx)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO records(`+recordCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
			ON CONFLICT(book_id) DO UPDATE SET
				file_name = excluded.file_name, file_size = excluded.file_size, title = excluded.title,
				last_page = excluded.last_page, page_count = excluded.page_count, progress = excluded.progress,
				updated_at = excluded.updated_at, deleted = excluded.deleted,
				updated_by = excluded.updated_by, rev = excluded.rev`,
			r.BookID, r.FileName, r.FileSize, r.Title, r.LastPage, r.PageCount, r.Progress,
			r.UpdatedAt, r.Deleted, deviceID, rev)
		if err != nil {
			return nil, err
		}
		saved, err := getRecord(ctx, tx, r.BookID)
		if err != nil {
			return nil, err
		}
		out = append(out, saved)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// Changes returns up to limit records with rev > since in rev order, and
// whether more follow.
func (s *Store) Changes(ctx context.Context, since int64, limit int) ([]Record, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+recordCols+` FROM records WHERE rev > ? ORDER BY rev LIMIT ?`, since, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}

// MarkFileStored sets has_file and gives the record a new rev. It reports
// false if the record already had a file.
func (s *Store) MarkFileStored(ctx context.Context, bookID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	cur, err := getRecord(ctx, tx, bookID)
	if err != nil {
		return false, err
	}
	if cur.HasFile {
		return false, nil
	}
	rev, err := nextRev(ctx, tx)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE records SET has_file = 1, rev = ? WHERE book_id = ?`, rev, bookID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
