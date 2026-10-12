// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrBookmarkConflict means a pushed bookmark id already belongs to another book.
var ErrBookmarkConflict = errors.New("bookmark id belongs to another book")

// Bookmark is a bookmark. JSON names follow api/openapi.yaml.
type Bookmark struct {
	ID        string  `json:"id"`
	BookID    string  `json:"bookId"`
	Page      int64   `json:"page"`
	PageCount int64   `json:"pageCount"`
	Progress  float64 `json:"progress"`
	Note      *string `json:"note"`
	Snippet   *string `json:"snippet"`
	CreatedAt int64   `json:"createdAt"`
	UpdatedAt int64   `json:"updatedAt"`
	Deleted   bool    `json:"deleted"`
	UpdatedBy string  `json:"updatedBy"`
	Rev       int64   `json:"rev"`
}

const bookmarkCols = `id, book_id, page, page_count, progress, note, snippet, created_at, updated_at, deleted, updated_by, rev`

func scanBookmark(row scanner) (Bookmark, error) {
	var b Bookmark
	var note, snippet sql.NullString
	err := row.Scan(&b.ID, &b.BookID, &b.Page, &b.PageCount, &b.Progress, &note, &snippet,
		&b.CreatedAt, &b.UpdatedAt, &b.Deleted, &b.UpdatedBy, &b.Rev)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if note.Valid {
		b.Note = &note.String
	}
	if snippet.Valid {
		b.Snippet = &snippet.String
	}
	return b, err
}

func getBookmark(ctx context.Context, q querier, id string) (Bookmark, error) {
	return scanBookmark(q.QueryRowContext(ctx, `SELECT `+bookmarkCols+` FROM bookmarks WHERE id = ?`, id))
}

// PushBookmarks stores each bookmark that is new or has a strictly greater
// updatedAt than the stored one, all in one transaction, and returns what is
// stored afterwards, one per input in the same order. A deleted bookmark is
// kept as a tombstone. If an id is already used by another book, nothing is
// stored and ErrBookmarkConflict is returned. UpdatedBy and Rev of the
// inputs are ignored.
func (s *Store) PushBookmarks(ctx context.Context, deviceID string, in []Bookmark) ([]Bookmark, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := make([]Bookmark, 0, len(in))
	for _, b := range in {
		cur, err := getBookmark(ctx, tx, b.ID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return nil, err
		case cur.BookID != b.BookID:
			return nil, ErrBookmarkConflict
		case b.UpdatedAt <= cur.UpdatedAt:
			out = append(out, cur)
			continue
		}
		rev, err := nextRev(ctx, tx)
		if err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO bookmarks(`+bookmarkCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				page = excluded.page, page_count = excluded.page_count, progress = excluded.progress,
				note = excluded.note, snippet = excluded.snippet, created_at = excluded.created_at,
				updated_at = excluded.updated_at, deleted = excluded.deleted,
				updated_by = excluded.updated_by, rev = excluded.rev`,
			b.ID, b.BookID, b.Page, b.PageCount, b.Progress, b.Note, b.Snippet,
			b.CreatedAt, b.UpdatedAt, b.Deleted, deviceID, rev)
		if err != nil {
			return nil, err
		}
		saved, err := getBookmark(ctx, tx, b.ID)
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

// BookmarkChanges returns up to limit bookmarks, tombstones included, with
// rev > since in rev order, and whether more follow.
func (s *Store) BookmarkChanges(ctx context.Context, since int64, limit int) ([]Bookmark, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+bookmarkCols+` FROM bookmarks WHERE rev > ? ORDER BY rev LIMIT ?`, since, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []Bookmark{}
	for rows.Next() {
		b, err := scanBookmark(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > limit {
		return out[:limit], true, nil
	}
	return out, false, nil
}
