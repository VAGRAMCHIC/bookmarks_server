// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"fmt"
	"strings"
	"testing"
)

func bm(id int, book string, page int, updatedAt int64, deleted bool) map[string]any {
	return map[string]any{
		"id": fmt.Sprintf("%032x", id), "bookId": book, "page": page, "pageCount": 100,
		"progress": float64(page) / 100, "note": "заметка", "snippet": "Глава 1",
		"createdAt": 1000, "updatedAt": updatedAt, "deleted": deleted,
	}
}

func bookmarks(t *testing.T, r response) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, x := range r.json(t)["bookmarks"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestBookmarksBetweenDevices(t *testing.T) {
	e := newEnv(t)
	a, b := e.paired(), e.paired()
	book1 := fmt.Sprintf("b1_%064x", 1)
	book2 := fmt.Sprintf("b1_%064x", 2)

	// A book record takes rev 1; bookmarks need no record and share the counter.
	expect(t, a.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(book1, 0, 1)}}, nil), 200, "")
	r := a.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": []any{bm(1, book1, 10, 2000, false), bm(2, book2, 5, 2000, false)}}, nil)
	expect(t, r, 200, "")
	got := bookmarks(t, r)
	if len(got) != 2 || got[0]["rev"] != 2.0 || got[1]["rev"] != 3.0 || got[0]["updatedBy"] != a.id || got[0]["note"] != "заметка" {
		t.Fatalf("push = %v", got)
	}

	m := b.do("GET", "/v1/bookmarks/changes", nil, nil).json(t)
	if m["cursor"] != 3.0 || m["hasMore"] != false || len(m["bookmarks"].([]any)) != 2 {
		t.Fatalf("B changes = %v", m)
	}
	// The records feed does not include bookmarks.
	if m := b.do("GET", "/v1/library/changes", nil, nil).json(t); m["cursor"] != 1.0 {
		t.Fatalf("records feed = %v", m)
	}

	// B deletes bookmark 1; a stale edit from A loses.
	r = b.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": []any{bm(1, book1, 10, 3000, true)}}, nil)
	if got := bookmarks(t, r)[0]; got["deleted"] != true || got["rev"] != 4.0 || got["updatedBy"] != b.id {
		t.Fatalf("delete = %v", got)
	}
	r = a.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": []any{bm(1, book1, 11, 3000, false)}}, nil)
	if got := bookmarks(t, r)[0]; got["deleted"] != true || got["rev"] != 4.0 {
		t.Fatalf("tie overwrote: %v", got)
	}

	// A sees the tombstone, then restores it with a later write.
	m = a.do("GET", "/v1/bookmarks/changes?since=3", nil, nil).json(t)
	if bms := m["bookmarks"].([]any); len(bms) != 1 || bms[0].(map[string]any)["deleted"] != true || m["cursor"] != 4.0 {
		t.Fatalf("A changes = %v", m)
	}
	r = a.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": []any{bm(1, book1, 10, 4000, false)}}, nil)
	if got := bookmarks(t, r)[0]; got["deleted"] != false || got["rev"] != 5.0 {
		t.Fatalf("restore = %v", got)
	}

	// Nulls and defaults.
	minimal := map[string]any{"id": fmt.Sprintf("%032x", 3), "bookId": book2, "page": 0, "pageCount": 0, "createdAt": 1, "updatedAt": 1}
	r = a.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": []any{minimal}}, nil)
	if got := bookmarks(t, r)[0]; got["note"] != nil || got["snippet"] != nil || got["progress"] != 0.0 || got["deleted"] != false {
		t.Fatalf("minimal = %v", got)
	}

	// Pagination.
	m = b.do("GET", "/v1/bookmarks/changes?since=0&limit=2", nil, nil).json(t)
	if m["hasMore"] != true || m["cursor"] != 5.0 {
		t.Fatalf("page 1 = %v", m)
	}
	m = b.do("GET", "/v1/bookmarks/changes?since=5&limit=2", nil, nil).json(t)
	if m["hasMore"] != false || m["cursor"] != 6.0 {
		t.Fatalf("page 2 = %v", m)
	}
	if m = b.do("GET", "/v1/bookmarks/changes?since=6", nil, nil).json(t); m["cursor"] != 6.0 || len(m["bookmarks"].([]any)) != 0 {
		t.Fatalf("empty page = %v", m)
	}

	// Same id, other book: 409 and nothing from the request is stored.
	r = a.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": []any{bm(4, book1, 1, 1, false), bm(1, book2, 1, 9000, false)}}, nil)
	expect(t, r, 409, "bookmark_conflict")
	if m = b.do("GET", "/v1/bookmarks/changes?since=6", nil, nil).json(t); len(m["bookmarks"].([]any)) != 0 {
		t.Fatalf("conflicting request stored: %v", m)
	}

	// Validation.
	long := strings.Repeat("я", 1001)
	for _, x := range []map[string]any{
		{"id": "XYZ", "bookId": book1, "page": 0, "pageCount": 0, "createdAt": 1, "updatedAt": 1},
		{"id": fmt.Sprintf("%032x", 5), "bookId": "b1_x", "page": 0, "pageCount": 0, "createdAt": 1, "updatedAt": 1},
		{"id": fmt.Sprintf("%032x", 5), "bookId": book1, "page": -1, "pageCount": 0, "createdAt": 1, "updatedAt": 1},
		{"id": fmt.Sprintf("%032x", 5), "bookId": book1, "page": 0, "pageCount": 0, "updatedAt": 1},
		{"id": fmt.Sprintf("%032x", 5), "bookId": book1, "page": 0, "pageCount": 0, "createdAt": 1, "updatedAt": 1, "progress": 2},
		{"id": fmt.Sprintf("%032x", 5), "bookId": book1, "page": 0, "pageCount": 0, "createdAt": 1, "updatedAt": 1, "note": long},
		{"id": fmt.Sprintf("%032x", 5), "bookId": book1, "page": 0, "pageCount": 0, "createdAt": 1, "updatedAt": 1, "snippet": long[:402]},
	} {
		expect(t, a.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": []any{x}}, nil), 400, "bad_request")
	}
	many := make([]any, 501)
	for i := range many {
		many[i] = bm(100+i, book1, 0, 1, false)
	}
	expect(t, a.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": many}, nil), 400, "bad_request")
	expect(t, a.do("POST", "/v1/bookmarks", map[string]any{}, nil), 400, "bad_request")
	expect(t, a.do("GET", "/v1/bookmarks/changes?limit=1001", nil, nil), 400, "bad_request")

	// Unpaired devices get 401.
	c := e.newClient()
	expect(t, c.do("GET", "/v1/bookmarks/changes", nil, nil), 401, "unknown_device")
	expect(t, c.do("POST", "/v1/bookmarks", map[string]any{"bookmarks": []any{}}, nil), 401, "unknown_device")
}
