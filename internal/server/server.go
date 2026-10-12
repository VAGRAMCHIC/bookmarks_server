// SPDX-License-Identifier: AGPL-3.0-or-later

// Package server implements the HTTP API described in api/openapi.yaml.
package server

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/vagramchic/bookmarks-server/internal/certs"
	"github.com/vagramchic/bookmarks-server/internal/store"
)

// MaxJSONBody limits JSON request bodies.
const MaxJSONBody = 2 << 20

// DefaultMaxFileSize is the default limit for uploaded book files.
const DefaultMaxFileSize = 500 << 20

// Config configures a Server.
type Config struct {
	Store             *store.Store
	FilesDir          string // book files live here
	Name              string // shown to clients as serverName
	ServerFingerprint string // fingerprint of the server's own certificate
	MaxFileSize       int64
	MaxDevices        int // paired devices allowed; 0 means no limit
	Logger            *slog.Logger
	Now               func() time.Time // for tests; time.Now by default

	PairLimit  int           // POST /v1/pair requests per window per IP; 5 by default
	PairWindow time.Duration // 1 minute by default
}

// Server serves the sync API.
type Server struct {
	cfg     Config
	log     *slog.Logger
	limiter *rateLimiter
	mux     *http.ServeMux

	seenMu sync.Mutex
	seen   map[string]time.Time // device id -> last TouchDevice
}

// New returns a Server for cfg.
func New(cfg Config) *Server {
	if cfg.MaxFileSize <= 0 {
		cfg.MaxFileSize = DefaultMaxFileSize
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PairLimit <= 0 {
		cfg.PairLimit = 5
	}
	if cfg.PairWindow <= 0 {
		cfg.PairWindow = time.Minute
	}
	s := &Server{
		cfg:     cfg,
		log:     cfg.Logger,
		limiter: newRateLimiter(cfg.PairLimit, cfg.PairWindow),
		mux:     http.NewServeMux(),
		seen:    map[string]time.Time{},
	}
	s.mux.HandleFunc("POST /v1/pair", s.handlePair)
	s.mux.HandleFunc("GET /v1/info", s.auth(s.handleInfo))
	s.mux.HandleFunc("GET /v1/devices", s.auth(s.handleDevices))
	s.mux.HandleFunc("DELETE /v1/devices/me", s.auth(s.handleUnpairSelf))
	s.mux.HandleFunc("DELETE /v1/devices/{deviceId}", s.auth(s.handleRemoveDevice))
	s.mux.HandleFunc("GET /v1/library/changes", s.auth(s.handleChanges))
	s.mux.HandleFunc("POST /v1/library/records", s.auth(s.handlePushRecords))
	s.mux.HandleFunc("GET /v1/bookmarks/changes", s.auth(s.handleBookmarkChanges))
	s.mux.HandleFunc("POST /v1/bookmarks", s.auth(s.handlePushBookmarks))
	s.mux.HandleFunc("PUT /v1/books/{bookId}/file", s.auth(s.handleUpload))
	s.mux.HandleFunc("GET /v1/books/{bookId}/file", s.auth(s.handleDownload)) // also HEAD
	s.mux.HandleFunc("/", s.auth(func(w http.ResponseWriter, r *http.Request, _ store.Device) {
		writeError(w, http.StatusNotFound, "not_found", "Нет такого метода API")
	}))
	return s
}

// TLSConfig returns the server TLS configuration: any client certificate is
// accepted at the TLS level and checked by fingerprint in the handlers.
func TLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
	}
}

// HTTPServer wraps h in an http.Server with the timeouts the API needs.
// There is no overall read timeout so large uploads can finish.
func HTTPServer(addr string, h http.Handler, tlsConfig *tls.Config, log *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		TLSConfig:         tlsConfig,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	var device string // filled in by auth for the request log
	r = r.WithContext(context.WithValue(r.Context(), deviceKey{}, &device))
	s.mux.ServeHTTP(sw, r)
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", sw.status,
		"duration", time.Since(start).Round(time.Millisecond),
		"remote", remoteIP(r),
	}
	if device != "" {
		attrs = append(attrs, "device", device)
	}
	s.log.Info("request", attrs...)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// ----- authentication -----

type deviceKey struct{}

func clientFingerprint(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", false
	}
	return certs.Fingerprint(r.TLS.PeerCertificates[0].Raw), true
}

// auth lets the request through only for a paired device.
func (s *Server) auth(h func(http.ResponseWriter, *http.Request, store.Device)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fp, ok := clientFingerprint(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unknown_device", "Устройство не предъявило сертификат. Сопрягите его с сервером заново.")
			return
		}
		dev, err := s.cfg.Store.DeviceByFingerprint(r.Context(), fp)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusUnauthorized, "unknown_device", "Сервер не знает это устройство. Сопрягите его заново.")
			return
		}
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		if p, ok := r.Context().Value(deviceKey{}).(*string); ok {
			*p = dev.ID
		}
		s.touch(r.Context(), dev.ID)
		h(w, r, dev)
	}
}

// touch updates last_seen_at at most once a minute per device.
func (s *Server) touch(ctx context.Context, id string) {
	now := s.cfg.Now()
	s.seenMu.Lock()
	last, ok := s.seen[id]
	if ok && now.Sub(last) < time.Minute {
		s.seenMu.Unlock()
		return
	}
	s.seen[id] = now
	s.seenMu.Unlock()
	if err := s.cfg.Store.TouchDevice(ctx, id, now); err != nil {
		s.log.Warn("update last_seen_at", "device", id, "err", err)
	}
}

// ----- responses -----

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	var e apiError
	e.Error.Code = code
	e.Error.Message = message
	writeJSON(w, status, e)
}

func (s *Server) internalError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("internal error", "method", r.Method, "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal", "Внутренняя ошибка сервера")
}

// decodeJSON reads a JSON body of at most MaxJSONBody bytes. On failure it
// writes the error response and returns false.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, MaxJSONBody)
	err := json.NewDecoder(body).Decode(v)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "Запрос слишком большой")
		return false
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_request", "Неверный JSON в запросе")
		return false
	}
	return true
}

func badRequest(w http.ResponseWriter, message string) {
	writeError(w, http.StatusBadRequest, "bad_request", message)
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
