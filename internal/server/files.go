// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"github.com/vagramchic/bookmarks-server/internal/bookid"
	"github.com/vagramchic/bookmarks-server/internal/store"
)

// filePath is files/ab/cd/<bookId>, sharded by the first hash characters.
func (s *Server) filePath(id string) string {
	return filepath.Join(s.cfg.FilesDir, id[3:5], id[5:7], id)
}

// errWriter remembers write errors so they can be told apart from read
// errors after io.Copy.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, dev store.Device) {
	id := r.PathValue("bookId")
	if !bookid.Valid(id) {
		writeError(w, http.StatusNotFound, "not_found", "Неверный bookId")
		return
	}
	if r.ContentLength < 0 {
		writeError(w, http.StatusLengthRequired, "length_required", "Нужен заголовок Content-Length")
		return
	}
	if r.ContentLength > s.cfg.MaxFileSize {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "Файл больше, чем разрешено на сервере")
		return
	}
	if _, err := s.cfg.Store.Record(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Сначала отправьте запись об этой книге")
		return
	} else if err != nil {
		s.internalError(w, r, err)
		return
	}
	body := http.MaxBytesReader(w, r.Body, r.ContentLength)
	final := s.filePath(id)

	if _, err := os.Stat(final); err == nil {
		// Already stored: read the body so the connection can be reused.
		io.Copy(io.Discard, body)
		if _, err := s.cfg.Store.MarkFileStored(r.Context(), id); err != nil {
			s.internalError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.storageError(w, r, err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		s.storageError(w, r, err)
		return
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name()) // no-op after a successful rename
	}()

	hash := bookid.NewWriter(r.ContentLength)
	fw := &errWriter{w: tmp}
	n, err := io.Copy(io.MultiWriter(fw, hash), body)
	switch {
	case fw.err != nil:
		s.storageError(w, r, fw.err)
		return
	case err != nil:
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "Тело запроса длиннее Content-Length")
			return
		}
		s.log.Warn("upload aborted", "book", id, "device", dev.ID, "err", err)
		badRequest(w, "Загрузка прервалась")
		return
	case n != r.ContentLength:
		badRequest(w, "Получено меньше данных, чем указано в Content-Length")
		return
	}
	if got := hash.Sum(); got != id {
		writeError(w, http.StatusUnprocessableEntity, "book_id_mismatch", "Содержимое файла не соответствует bookId")
		return
	}
	if err := tmp.Sync(); err != nil {
		s.storageError(w, r, err)
		return
	}
	if err := tmp.Close(); err != nil {
		s.storageError(w, r, err)
		return
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		s.storageError(w, r, err)
		return
	}
	if _, err := s.cfg.Store.MarkFileStored(r.Context(), id); err != nil {
		s.internalError(w, r, err)
		return
	}
	s.log.Info("file stored", "book", id, "device", dev.ID, "size", n)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) storageError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, syscall.ENOSPC) {
		s.log.Error("disk full", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInsufficientStorage, "insufficient_storage", "На сервере закончилось место")
		return
	}
	s.internalError(w, r, err)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, _ store.Device) {
	id := r.PathValue("bookId")
	if !bookid.Valid(id) {
		writeError(w, http.StatusNotFound, "not_found", "Неверный bookId")
		return
	}
	f, err := os.Open(s.filePath(id))
	if errors.Is(err, fs.ErrNotExist) {
		writeError(w, http.StatusNotFound, "not_found", "Файла этой книги нет на сервере")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+id+`"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}
