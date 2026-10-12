// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/vagramchic/bookmarks-server/internal/bookid"
	"github.com/vagramchic/bookmarks-server/internal/store"
)

// MaxBookmarksPerPush limits POST /v1/bookmarks.
const MaxBookmarksPerPush = 500

var bookmarkID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type bookmarkInput struct {
	ID        *string  `json:"id"`
	BookID    *string  `json:"bookId"`
	Page      *int64   `json:"page"`
	PageCount *int64   `json:"pageCount"`
	Progress  *float64 `json:"progress"`
	Note      *string  `json:"note"`
	Snippet   *string  `json:"snippet"`
	CreatedAt *int64   `json:"createdAt"`
	UpdatedAt *int64   `json:"updatedAt"`
	Deleted   *bool    `json:"deleted"`
}

// validate checks in against BookmarkInput and returns the bookmark, or a
// message for the user.
func (in bookmarkInput) validate() (store.Bookmark, string) {
	switch {
	case in.ID == nil || in.BookID == nil || in.Page == nil || in.PageCount == nil || in.CreatedAt == nil || in.UpdatedAt == nil:
		return store.Bookmark{}, "У закладки нет обязательных полей id, bookId, page, pageCount, createdAt или updatedAt"
	case !bookmarkID.MatchString(*in.ID):
		return store.Bookmark{}, "Неверный id закладки: " + truncate(*in.ID, 80)
	case !bookid.Valid(*in.BookID):
		return store.Bookmark{}, "Неверный bookId: " + truncate(*in.BookID, 80)
	case *in.Page < 0 || *in.PageCount < 0:
		return store.Bookmark{}, "Номер страницы и число страниц не могут быть отрицательными"
	case in.Progress != nil && !(*in.Progress >= 0 && *in.Progress <= 1):
		return store.Bookmark{}, "progress должен быть от 0 до 1"
	case in.Note != nil && utf8.RuneCountInString(*in.Note) > 1000:
		return store.Bookmark{}, "Заметка длиннее 1000 символов"
	case in.Snippet != nil && utf8.RuneCountInString(*in.Snippet) > 200:
		return store.Bookmark{}, "Фрагмент текста длиннее 200 символов"
	}
	return store.Bookmark{
		ID:        *in.ID,
		BookID:    *in.BookID,
		Page:      *in.Page,
		PageCount: *in.PageCount,
		Progress:  deref(in.Progress),
		Note:      in.Note,
		Snippet:   in.Snippet,
		CreatedAt: *in.CreatedAt,
		UpdatedAt: *in.UpdatedAt,
		Deleted:   deref(in.Deleted),
	}, ""
}

func (s *Server) handlePushBookmarks(w http.ResponseWriter, r *http.Request, dev store.Device) {
	var req struct {
		Bookmarks *[]bookmarkInput `json:"bookmarks"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Bookmarks == nil {
		badRequest(w, "Нет поля bookmarks")
		return
	}
	if len(*req.Bookmarks) > MaxBookmarksPerPush {
		badRequest(w, "Не больше 500 закладок за один запрос")
		return
	}
	in := make([]store.Bookmark, 0, len(*req.Bookmarks))
	for i, x := range *req.Bookmarks {
		b, msg := x.validate()
		if msg != "" {
			badRequest(w, "Закладка "+strconv.Itoa(i+1)+": "+msg)
			return
		}
		in = append(in, b)
	}
	out, err := s.cfg.Store.PushBookmarks(r.Context(), dev.ID, in)
	if errors.Is(err, store.ErrBookmarkConflict) {
		writeError(w, http.StatusConflict, "bookmark_conflict", "Закладка с таким id уже относится к другой книге")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bookmarks": out})
}

type bookmarkChangesResponse struct {
	Bookmarks []store.Bookmark `json:"bookmarks"`
	Cursor    int64            `json:"cursor"`
	HasMore   bool             `json:"hasMore"`
}

func (s *Server) handleBookmarkChanges(w http.ResponseWriter, r *http.Request, _ store.Device) {
	since, limit, ok := pageParams(w, r)
	if !ok {
		return
	}
	bms, more, err := s.cfg.Store.BookmarkChanges(r.Context(), since, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cursor := since
	if len(bms) > 0 {
		cursor = bms[len(bms)-1].Rev
	}
	writeJSON(w, http.StatusOK, bookmarkChangesResponse{Bookmarks: bms, Cursor: cursor, HasMore: more})
}
