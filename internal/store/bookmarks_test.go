// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func bmID(n int) string { return fmt.Sprintf("%032x", n) }

func TestBookmarks(t *testing.T) {
	s, _ := open(t)
	note := "важное место"
	// Records and bookmarks share the rev counter.
	if _, err := s.PushRecords(ctx, "a", []Record{{BookID: bid(1), FileName: "f", UpdatedAt: 1}}); err != nil {
		t.Fatal(err)
	}
	out, err := s.PushBookmarks(ctx, "a", []Bookmark{
		{ID: bmID(1), BookID: bid(1), Page: 3, PageCount: 10, Progress: 0.3, Note: &note, CreatedAt: 10, UpdatedAt: 10},
		{ID: bmID(2), BookID: bid(9), Page: 0, PageCount: 5, CreatedAt: 10, UpdatedAt: 10}, // no such book: fine
	})
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Rev != 2 || out[1].Rev != 3 || *out[0].Note != note || out[1].Note != nil || out[0].UpdatedBy != "a" {
		t.Fatalf("push = %+v", out)
	}

	// Equal and older updatedAt keep the stored version.
	for _, at := range []int64{9, 10} {
		out, err := s.PushBookmarks(ctx, "b", []Bookmark{{ID: bmID(1), BookID: bid(1), Page: 7, CreatedAt: 10, UpdatedAt: at}})
		if err != nil || out[0].Page != 3 || out[0].Rev != 2 {
			t.Fatalf("updatedAt %d: %+v %v", at, out, err)
		}
	}

	// Delete is a tombstone; a later write restores it.
	out, err = s.PushBookmarks(ctx, "b", []Bookmark{{ID: bmID(1), BookID: bid(1), Page: 3, CreatedAt: 10, UpdatedAt: 20, Deleted: true}})
	if err != nil || !out[0].Deleted || out[0].Rev != 4 || out[0].UpdatedBy != "b" || out[0].Note != nil {
		t.Fatalf("delete = %+v %v", out, err)
	}
	out, err = s.PushBookmarks(ctx, "a", []Bookmark{{ID: bmID(1), BookID: bid(1), Page: 3, CreatedAt: 10, UpdatedAt: 30}})
	if err != nil || out[0].Deleted || out[0].Rev != 5 {
		t.Fatalf("restore = %+v %v", out, err)
	}

	// An id reused for another book fails the whole request.
	_, err = s.PushBookmarks(ctx, "a", []Bookmark{
		{ID: bmID(3), BookID: bid(1), CreatedAt: 1, UpdatedAt: 1},
		{ID: bmID(1), BookID: bid(2), CreatedAt: 1, UpdatedAt: 99},
	})
	if !errors.Is(err, ErrBookmarkConflict) {
		t.Fatalf("conflict: %v", err)
	}

	// Feed: tombstones included, separate from the records feed, paginated.
	var revs []int64
	since := int64(0)
	for {
		page, more, err := s.BookmarkChanges(ctx, since, 1)
		if err != nil {
			t.Fatal(err)
		}
		for _, b := range page {
			revs = append(revs, b.Rev)
			since = b.Rev
		}
		if !more {
			break
		}
	}
	if fmt.Sprint(revs) != "[3 5]" {
		t.Fatalf("bookmark revs = %v (bmID(3) must not be stored)", revs)
	}
	recs, _, _ := s.Changes(ctx, 0, 100)
	if len(recs) != 1 {
		t.Fatalf("records feed has %d", len(recs))
	}
}

func TestMigrationFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), DBFile)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the database as schema 1 with some data.
	s.db.Exec(`DROP TABLE bookmarks; DROP TABLE records; DROP TABLE devices; DROP TABLE pairing_codes; DROP TABLE meta`)
	if err := s.migrate(ctx, migrations[:1]); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Version(ctx); v != 1 {
		t.Fatalf("version %d", v)
	}
	if _, err := s.PushRecords(ctx, "d", []Record{{BookID: bid(1), FileName: "f", UpdatedAt: 1}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.Version(ctx); v != SchemaVersion || SchemaVersion != 2 {
		t.Fatalf("version %d", v)
	}
	out, err := s.PushBookmarks(ctx, "d", []Bookmark{{ID: bmID(1), BookID: bid(1), CreatedAt: 1, UpdatedAt: 1}})
	if err != nil || out[0].Rev != 2 {
		t.Fatalf("after upgrade: %+v %v", out, err)
	}
}
