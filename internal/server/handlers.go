// SPDX-License-Identifier: AGPL-3.0-or-later

package server

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vagramchic/bookmarks-server/internal/bookid"
	"github.com/vagramchic/bookmarks-server/internal/pairing"
	"github.com/vagramchic/bookmarks-server/internal/store"
)

var hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ----- pairing -----

type pairRequest struct {
	DeviceName  *string `json:"deviceName"`
	Platform    *string `json:"platform"`
	ClientProof *string `json:"clientProof"`
}

type pairResponse struct {
	DeviceID    string `json:"deviceId"`
	ServerName  string `json:"serverName"`
	ServerProof string `json:"serverProof"`
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	clientFP, ok := clientFingerprint(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "no_client_certificate", "Устройство не предъявило сертификат")
		return
	}
	if wait, ok := s.limiter.allow(remoteIP(r), s.cfg.Now()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int((wait+time.Second-1)/time.Second)))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "Слишком много попыток сопряжения. Подождите минуту.")
		return
	}
	var req pairRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.DeviceName == nil || req.Platform == nil || req.ClientProof == nil:
		badRequest(w, "Не хватает полей deviceName, platform или clientProof")
		return
	case *req.DeviceName == "" || utf8.RuneCountInString(*req.DeviceName) > 100:
		badRequest(w, "Имя устройства должно быть от 1 до 100 символов")
		return
	case utf8.RuneCountInString(*req.Platform) > 100:
		badRequest(w, "Слишком длинное название платформы")
		return
	case !hex64.MatchString(*req.ClientProof):
		badRequest(w, "Неверный формат clientProof")
		return
	}

	dev, code, err := s.cfg.Store.Pair(r.Context(), store.PairRequest{
		Name:              *req.DeviceName,
		Platform:          *req.Platform,
		ServerFingerprint: s.cfg.ServerFingerprint,
		ClientFingerprint: clientFP,
		ClientProof:       *req.ClientProof,
		Now:               s.cfg.Now(),
		MaxDevices:        s.cfg.MaxDevices,
	})
	switch {
	case errors.Is(err, store.ErrAlreadyPaired):
		writeError(w, http.StatusConflict, "already_paired", "Это устройство уже сопряжено с сервером")
		return
	case errors.Is(err, store.ErrDeviceLimit):
		s.log.Warn("pairing refused: device limit", "max_devices", s.cfg.MaxDevices)
		writeError(w, http.StatusForbidden, "device_limit_reached",
			"К серверу уже подключено наибольшее разрешённое число устройств ("+strconv.Itoa(s.cfg.MaxDevices)+
				"). Отключите ненужное устройство и попробуйте снова с тем же кодом.")
		return
	case errors.Is(err, store.ErrInvalidCode):
		s.log.Warn("pairing failed", "remote", remoteIP(r))
		writeError(w, http.StatusForbidden, "invalid_pairing_code",
			"Код сопряжения неверный или устарел. Получите новый код на сервере: bookmarks-server pair")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	s.log.Info("device paired", "device", dev.ID, "name", dev.Name, "platform", dev.Platform)
	writeJSON(w, http.StatusOK, pairResponse{
		DeviceID:    dev.ID,
		ServerName:  s.cfg.Name,
		ServerProof: pairing.ServerProof(code, s.cfg.ServerFingerprint, clientFP),
	})
}

// ----- devices -----

type serverInfo struct {
	ServerName  string    `json:"serverName"`
	APIVersion  int       `json:"apiVersion"`
	DeviceID    string    `json:"deviceId"`
	Time        time.Time `json:"time"`
	MaxFileSize int64     `json:"maxFileSize"`
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request, dev store.Device) {
	writeJSON(w, http.StatusOK, serverInfo{
		ServerName:  s.cfg.Name,
		APIVersion:  1,
		DeviceID:    dev.ID,
		Time:        s.cfg.Now().UTC().Truncate(time.Millisecond),
		MaxFileSize: s.cfg.MaxFileSize,
	})
}

type deviceJSON struct {
	DeviceID    string     `json:"deviceId"`
	DeviceName  string     `json:"deviceName"`
	Platform    string     `json:"platform"`
	Fingerprint string     `json:"fingerprint"`
	PairedAt    time.Time  `json:"pairedAt"`
	LastSeenAt  *time.Time `json:"lastSeenAt"`
	Current     bool       `json:"current"`
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request, me store.Device) {
	devs, err := s.cfg.Store.Devices(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]deviceJSON, 0, len(devs))
	for _, d := range devs {
		out = append(out, deviceJSON{
			DeviceID:    d.ID,
			DeviceName:  d.Name,
			Platform:    d.Platform,
			Fingerprint: d.Fingerprint,
			PairedAt:    d.PairedAt,
			LastSeenAt:  d.LastSeenAt,
			Current:     d.ID == me.ID,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

func (s *Server) handleUnpairSelf(w http.ResponseWriter, r *http.Request, me store.Device) {
	s.removeDevice(w, r, me.ID)
}

func (s *Server) handleRemoveDevice(w http.ResponseWriter, r *http.Request, _ store.Device) {
	s.removeDevice(w, r, r.PathValue("deviceId"))
}

func (s *Server) removeDevice(w http.ResponseWriter, r *http.Request, id string) {
	err := s.cfg.Store.RemoveDevice(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", "Устройство не найдено")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.seenMu.Lock()
	delete(s.seen, id)
	s.seenMu.Unlock()
	s.log.Info("device removed", "device", id)
	w.WriteHeader(http.StatusNoContent)
}

// ----- library -----

type changesResponse struct {
	Records []store.Record `json:"records"`
	Cursor  int64          `json:"cursor"`
	HasMore bool           `json:"hasMore"`
}

func (s *Server) handleChanges(w http.ResponseWriter, r *http.Request, _ store.Device) {
	q := r.URL.Query()
	since, limit := int64(0), 500
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			badRequest(w, "since должен быть целым числом не меньше 0")
			return
		}
		since = n
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			badRequest(w, "limit должен быть от 1 до 1000")
			return
		}
		limit = n
	}
	recs, more, err := s.cfg.Store.Changes(r.Context(), since, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cursor := since
	if len(recs) > 0 {
		cursor = recs[len(recs)-1].Rev
	}
	writeJSON(w, http.StatusOK, changesResponse{Records: recs, Cursor: cursor, HasMore: more})
}

// MaxRecordsPerPush limits POST /v1/library/records.
const MaxRecordsPerPush = 500

type recordInput struct {
	BookID    *string  `json:"bookId"`
	FileName  *string  `json:"fileName"`
	FileSize  *int64   `json:"fileSize"`
	Title     *string  `json:"title"`
	LastPage  *int64   `json:"lastPage"`
	PageCount *int64   `json:"pageCount"`
	Progress  *float64 `json:"progress"`
	UpdatedAt *int64   `json:"updatedAt"`
	Deleted   *bool    `json:"deleted"`
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// validate checks in against BookRecordInput and returns the record, or a
// message for the user.
func (in recordInput) validate() (store.Record, string) {
	switch {
	case in.BookID == nil || in.FileName == nil || in.FileSize == nil || in.UpdatedAt == nil:
		return store.Record{}, "У записи нет обязательных полей bookId, fileName, fileSize или updatedAt"
	case !bookid.Valid(*in.BookID):
		return store.Record{}, "Неверный bookId: " + truncate(*in.BookID, 80)
	case utf8.RuneCountInString(*in.FileName) > 255 || strings.ContainsAny(*in.FileName, `/\`):
		return store.Record{}, "Неверное имя файла: не больше 255 символов и без / и \\"
	case *in.FileSize < 0:
		return store.Record{}, "Размер файла не может быть отрицательным"
	case in.Title != nil && utf8.RuneCountInString(*in.Title) > 1000:
		return store.Record{}, "Название длиннее 1000 символов"
	case deref(in.LastPage) < 0 || deref(in.PageCount) < 0:
		return store.Record{}, "Номер страницы и число страниц не могут быть отрицательными"
	case in.Progress != nil && !(*in.Progress >= 0 && *in.Progress <= 1):
		return store.Record{}, "progress должен быть от 0 до 1"
	}
	return store.Record{
		BookID:    *in.BookID,
		FileName:  *in.FileName,
		FileSize:  *in.FileSize,
		Title:     in.Title,
		LastPage:  deref(in.LastPage),
		PageCount: deref(in.PageCount),
		Progress:  deref(in.Progress),
		UpdatedAt: *in.UpdatedAt,
		Deleted:   deref(in.Deleted),
	}, ""
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func (s *Server) handlePushRecords(w http.ResponseWriter, r *http.Request, dev store.Device) {
	var req struct {
		Records *[]recordInput `json:"records"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Records == nil {
		badRequest(w, "Нет поля records")
		return
	}
	if len(*req.Records) > MaxRecordsPerPush {
		badRequest(w, "Не больше 500 записей за один запрос")
		return
	}
	recs := make([]store.Record, 0, len(*req.Records))
	for i, in := range *req.Records {
		rec, msg := in.validate()
		if msg != "" {
			badRequest(w, "Запись "+strconv.Itoa(i+1)+": "+msg)
			return
		}
		recs = append(recs, rec)
	}
	out, err := s.cfg.Store.PushRecords(r.Context(), dev.ID, recs)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": out})
}
