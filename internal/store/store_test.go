// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vagramchic/bookmarks-server/internal/pairing"
)

var ctx = context.Background()

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), DBFile)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func bid(n int) string { return fmt.Sprintf("b1_%064x", n) }

func TestMigrations(t *testing.T) {
	s, path := open(t)
	if v, err := s.Version(ctx); err != nil || v != SchemaVersion {
		t.Fatalf("version = %d, %v", v, err)
	}
	if _, err := s.PushRecords(ctx, "d", []Record{{BookID: bid(1), FileName: "a.pdf", UpdatedAt: 1}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Reopening is a no-op and keeps data and the rev counter.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s2.PushRecords(ctx, "d", []Record{{BookID: bid(2), FileName: "b.pdf", UpdatedAt: 1}})
	if err != nil || out[0].Rev != 2 {
		t.Fatalf("after reopen rev = %v, %v", out, err)
	}

	// A database from a newer server is refused.
	if _, err := s2.db.Exec(`UPDATE meta SET value = '999' WHERE key = 'schema_version'`); err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("opened a newer schema")
	}
}

func TestConflictResolution(t *testing.T) {
	s, _ := open(t)
	title := "Книга"
	first, err := s.PushRecords(ctx, "dev-a", []Record{{BookID: bid(1), FileName: "a.pdf", FileSize: 10, Title: &title, LastPage: 5, UpdatedAt: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if r := first[0]; r.Rev != 1 || r.UpdatedBy != "dev-a" || r.LastPage != 5 || *r.Title != title {
		t.Fatalf("first = %+v", r)
	}

	// Older and equal updatedAt lose; the stored record comes back.
	for _, at := range []int64{99, 100} {
		out, err := s.PushRecords(ctx, "dev-b", []Record{{BookID: bid(1), FileName: "x.pdf", LastPage: 9, UpdatedAt: at}})
		if err != nil {
			t.Fatal(err)
		}
		if r := out[0]; r.LastPage != 5 || r.UpdatedBy != "dev-a" || r.Rev != 1 {
			t.Fatalf("updatedAt %d overwrote: %+v", at, r)
		}
	}

	// has_file survives a newer record.
	if ok, err := s.MarkFileStored(ctx, bid(1)); !ok || err != nil {
		t.Fatalf("MarkFileStored = %v, %v", ok, err)
	}
	if ok, _ := s.MarkFileStored(ctx, bid(1)); ok {
		t.Fatal("second MarkFileStored changed the record")
	}
	out, err := s.PushRecords(ctx, "dev-b", []Record{
		{BookID: bid(1), FileName: "a.pdf", LastPage: 7, UpdatedAt: 101, HasFile: false, Deleted: true},
		{BookID: bid(2), FileName: "b.pdf", UpdatedAt: 1},
		{BookID: bid(1), FileName: "a.pdf", LastPage: 1, UpdatedAt: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	if r := out[0]; r.LastPage != 7 || !r.HasFile || !r.Deleted || r.UpdatedBy != "dev-b" || r.Rev != 3 || r.Title != nil {
		t.Fatalf("newer = %+v", r)
	}
	if out[1].Rev != 4 || out[2].Rev != 3 || out[2].LastPage != 7 {
		t.Fatalf("order/rev wrong: %+v", out)
	}
	if _, err := s.MarkFileStored(ctx, bid(9)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing record: %v", err)
	}
}

func TestChangesPagination(t *testing.T) {
	s, _ := open(t)
	var in []Record
	for i := range 7 {
		in = append(in, Record{BookID: bid(i), FileName: "f", UpdatedAt: 1})
	}
	if _, err := s.PushRecords(ctx, "d", in); err != nil {
		t.Fatal(err)
	}
	// Re-pushing book 0 moves it to the end of the feed.
	if _, err := s.PushRecords(ctx, "d", []Record{{BookID: bid(0), FileName: "f", UpdatedAt: 2}}); err != nil {
		t.Fatal(err)
	}
	var revs []int64
	var books []string
	since := int64(0)
	for {
		page, more, err := s.Changes(ctx, since, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			revs = append(revs, r.Rev)
			books = append(books, r.BookID[len(r.BookID)-1:])
			since = r.Rev
		}
		if !more {
			break
		}
	}
	if got := fmt.Sprint(revs); got != "[2 3 4 5 6 7 8]" {
		t.Fatalf("revs = %s", got)
	}
	if got := strings.Join(books, ""); got != "1234560" {
		t.Fatalf("books = %s", got)
	}
	page, more, err := s.Changes(ctx, 8, 3)
	if err != nil || len(page) != 0 || more {
		t.Fatalf("past end: %v %v %v", page, more, err)
	}
}

func TestPairing(t *testing.T) {
	s, _ := open(t)
	now := time.Now()
	sfp, cfp := strings.Repeat("11", 32), strings.Repeat("22", 32)
	req := func(code, client string, at time.Time) PairRequest {
		return PairRequest{Name: "Телефон", Platform: "android", ServerFingerprint: sfp, ClientFingerprint: client,
			ClientProof: pairing.ClientProof(code, sfp, client), Now: at}
	}

	if _, _, err := s.Pair(ctx, req("AAAA", cfp, now)); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("no code: %v", err)
	}

	code, _ := pairing.NewCode()
	if err := s.SetPairingCode(ctx, code, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Pair(ctx, req(code, cfp, now.Add(11*time.Minute))); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired: %v", err)
	}
	// The expired code is gone even for the right time now.
	if _, _, err := s.Pair(ctx, req(code, cfp, now)); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("expired code reused: %v", err)
	}

	code, _ = pairing.NewCode()
	s.SetPairingCode(ctx, code, now.Add(10*time.Minute))
	// A lower-case dashed code is the same code.
	d, key, err := s.Pair(ctx, req(strings.ToLower(pairing.Format(code)), cfp, now))
	if err != nil || key != code {
		t.Fatalf("pair: %v %q", err, key)
	}
	if !regexp.MustCompile(`^[a-z2-7]{26}$`).MatchString(d.ID) {
		t.Fatalf("device id %q", d.ID)
	}
	if _, _, err := s.Pair(ctx, req(code, strings.Repeat("33", 32), now)); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("reused code: %v", err)
	}
	if _, _, err := s.Pair(ctx, req(code, cfp, now)); !errors.Is(err, ErrAlreadyPaired) {
		t.Fatalf("already paired: %v", err)
	}
	got, err := s.DeviceByFingerprint(ctx, cfp)
	if err != nil || got.ID != d.ID || got.LastSeenAt != nil {
		t.Fatalf("lookup: %+v %v", got, err)
	}

	// Ten wrong proofs burn the code.
	code, _ = pairing.NewCode()
	s.SetPairingCode(ctx, code, now.Add(10*time.Minute))
	other := strings.Repeat("44", 32)
	for i := range MaxFailedAttempts {
		if _, _, err := s.Pair(ctx, req("WRONGWRONGWRONG0", other, now)); !errors.Is(err, ErrInvalidCode) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if _, _, err := s.Pair(ctx, req(code, other, now)); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("code survived 10 failures: %v", err)
	}

	// last_seen_at is written at most once a minute.
	s.TouchDevice(ctx, d.ID, now)
	s.TouchDevice(ctx, d.ID, now.Add(30*time.Second))
	got, _ = s.DeviceByFingerprint(ctx, cfp)
	if got.LastSeenAt == nil || got.LastSeenAt.UnixMilli() != now.UnixMilli() {
		t.Fatalf("last seen %v", got.LastSeenAt)
	}
	s.TouchDevice(ctx, d.ID, now.Add(61*time.Second))
	got, _ = s.DeviceByFingerprint(ctx, cfp)
	if got.LastSeenAt.UnixMilli() != now.Add(61*time.Second).UnixMilli() {
		t.Fatalf("last seen not updated %v", got.LastSeenAt)
	}

	if err := s.RemoveDevice(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveDevice(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
}
