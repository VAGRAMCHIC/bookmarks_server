// SPDX-License-Identifier: AGPL-3.0-or-later

package server_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/vagramchic/bookmarks-server/internal/bookid"
	"github.com/vagramchic/bookmarks-server/internal/certs"
	"github.com/vagramchic/bookmarks-server/internal/pairing"
	"github.com/vagramchic/bookmarks-server/internal/server"
	"github.com/vagramchic/bookmarks-server/internal/store"
)

type env struct {
	t        *testing.T
	ts       *httptest.Server
	st       *store.Store
	serverFP string
	router   routers.Router
}

type option func(*server.Config)

func newEnv(t *testing.T, opts ...option) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, store.DBFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cert, _, err := certs.LoadOrCreate(dir, "Тест")
	if err != nil {
		t.Fatal(err)
	}
	cfg := server.Config{
		Store:             st,
		FilesDir:          filepath.Join(dir, "files"),
		Name:              "Тест",
		ServerFingerprint: certs.Fingerprint(cert.Certificate[0]),
		Logger:            slog.New(slog.DiscardHandler),
		PairLimit:         1000,
	}
	for _, o := range opts {
		o(&cfg)
	}
	ts := httptest.NewUnstartedServer(server.New(cfg))
	ts.TLS = server.TLSConfig(cert)
	ts.StartTLS()
	t.Cleanup(ts.Close)

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("openapi.yaml: %v", err)
	}
	doc.Servers = openapi3.Servers{{URL: ts.URL}}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, ts: ts, st: st, serverFP: cfg.ServerFingerprint, router: router}
}

// newCode stores a fresh pairing code, as `bookmarks-server pair` does.
func (e *env) newCode(ttl time.Duration) string {
	code, err := pairing.NewCode()
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.SetPairingCode(context.Background(), code, time.Now().Add(ttl)); err != nil {
		e.t.Fatal(err)
	}
	return pairing.Format(code)
}

type client struct {
	e    *env
	http *http.Client
	fp   string
	id   string
}

// newClient makes a device with its own self-signed certificate that
// accepts the server only by fingerprint.
func (e *env) newClient() *client {
	cert, err := certs.Generate("device")
	if err != nil {
		e.t.Fatal(err)
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if certs.Fingerprint(raw[0]) != e.serverFP {
				return errors.New("server fingerprint mismatch")
			}
			return nil
		},
	}}
	e.t.Cleanup(tr.CloseIdleConnections)
	return &client{e: e, http: &http.Client{Transport: tr}, fp: certs.Fingerprint(cert.Certificate[0])}
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.body, &m); err != nil {
		t.Fatalf("status %d, body %q: %v", r.status, r.body, err)
	}
	return m
}

func (r response) errorCode(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	code, _ := e["code"].(string)
	msg, _ := e["message"].(string)
	if code == "" || msg == "" {
		t.Fatalf("not an error body: %s", r.body)
	}
	return code
}

// do sends a request and checks the response against api/openapi.yaml.
func (c *client) do(method, path string, body any, header map[string]string) response {
	t := c.e.t
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case io.Reader:
		rd = b
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.e.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if _, isJSON := body.(map[string]any); isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	c.e.validate(req, resp, data)
	return response{status: resp.StatusCode, header: resp.Header, body: data}
}

func (e *env) validate(req *http.Request, resp *http.Response, body []byte) {
	e.t.Helper()
	route, params, err := e.router.FindRoute(req)
	if err != nil {
		e.t.Errorf("%s %s is not in openapi.yaml: %v", req.Method, req.URL.Path, err)
		return
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request: req, PathParams: params, Route: route,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		},
		Status:  resp.StatusCode,
		Header:  resp.Header,
		Body:    io.NopCloser(bytes.NewReader(body)),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		e.t.Errorf("%s %s -> %d does not match openapi.yaml: %v\nbody: %s", req.Method, req.URL.Path, resp.StatusCode, err, body)
	}
}

func (c *client) pairWith(code string) response {
	c.e.t.Helper()
	r := c.do("POST", "/v1/pair", map[string]any{
		"deviceName":  "Телефон",
		"platform":    "android",
		"clientProof": pairing.ClientProof(code, c.e.serverFP, c.fp),
	}, nil)
	if r.status == http.StatusOK {
		m := r.json(c.e.t)
		if m["serverProof"] != pairing.ServerProof(code, c.e.serverFP, c.fp) {
			c.e.t.Fatalf("bad serverProof: %v", m)
		}
		c.id = m["deviceId"].(string)
	}
	return r
}

// paired returns a new client that is already paired.
func (e *env) paired() *client {
	e.t.Helper()
	c := e.newClient()
	if r := c.pairWith(e.newCode(10 * time.Minute)); r.status != http.StatusOK {
		e.t.Fatalf("pair: %d %s", r.status, r.body)
	}
	return c
}

func expect(t *testing.T, r response, status int, code string) {
	t.Helper()
	if r.status != status {
		t.Fatalf("status %d, want %d; body %s", r.status, status, r.body)
	}
	if code != "" {
		if got := r.errorCode(t); got != code {
			t.Fatalf("error code %q, want %q", got, code)
		}
	}
}

func TestPairing(t *testing.T) {
	e := newEnv(t)

	t.Run("valid code", func(t *testing.T) {
		c := e.newClient()
		expect(t, c.do("GET", "/v1/info", nil, nil), 401, "unknown_device")
		code := e.newCode(10 * time.Minute)
		expect(t, c.pairWith(strings.ToLower(code)), 200, "")
		info := c.do("GET", "/v1/info", nil, nil)
		expect(t, info, 200, "")
		if m := info.json(t); m["deviceId"] != c.id || m["serverName"] != "Тест" || m["apiVersion"] != 1.0 {
			t.Fatalf("info = %v", m)
		}

		// Same certificate again.
		expect(t, c.pairWith(e.newCode(10*time.Minute)), 409, "already_paired")

		// The code from the successful pairing is spent.
		expect(t, e.newClient().pairWith(code), 403, "invalid_pairing_code")
	})

	t.Run("wrong code", func(t *testing.T) {
		e.newCode(10 * time.Minute)
		expect(t, e.newClient().pairWith("AAAA-BBBB-CCCC-DDDD"), 403, "invalid_pairing_code")
	})

	t.Run("expired code", func(t *testing.T) {
		code := e.newCode(-time.Second)
		expect(t, e.newClient().pairWith(code), 403, "invalid_pairing_code")
	})

	t.Run("no code", func(t *testing.T) {
		expect(t, e.newClient().pairWith("AAAA-BBBB-CCCC-DDDD"), 403, "invalid_pairing_code")
	})

	t.Run("ten failures burn the code", func(t *testing.T) {
		code := e.newCode(10 * time.Minute)
		c := e.newClient()
		for range 10 {
			expect(t, c.pairWith("AAAA-BBBB-CCCC-DDDD"), 403, "invalid_pairing_code")
		}
		expect(t, c.pairWith(code), 403, "invalid_pairing_code")
	})

	t.Run("new code replaces old", func(t *testing.T) {
		old := e.newCode(10 * time.Minute)
		code := e.newCode(10 * time.Minute)
		expect(t, e.newClient().pairWith(old), 403, "invalid_pairing_code")
		expect(t, e.newClient().pairWith(code), 200, "")
	})

	t.Run("bad request", func(t *testing.T) {
		c := e.newClient()
		expect(t, c.do("POST", "/v1/pair", map[string]any{"deviceName": "x", "platform": "linux", "clientProof": "zz"}, nil), 400, "bad_request")
		expect(t, c.do("POST", "/v1/pair", map[string]any{"platform": "linux", "clientProof": strings.Repeat("a", 64)}, nil), 400, "bad_request")
		expect(t, c.do("POST", "/v1/pair", []byte("{"), nil), 400, "bad_request")
	})
}

func TestPairRateLimit(t *testing.T) {
	e := newEnv(t, func(c *server.Config) { c.PairLimit = 0 }) // default: 5 per minute
	c := e.newClient()
	for range 5 {
		expect(t, c.pairWith("AAAA-BBBB-CCCC-DDDD"), 403, "invalid_pairing_code")
	}
	r := c.pairWith("AAAA-BBBB-CCCC-DDDD")
	expect(t, r, 429, "rate_limited")
	if s, err := strconv.Atoi(r.header.Get("Retry-After")); err != nil || s < 1 || s > 60 {
		t.Fatalf("Retry-After = %q", r.header.Get("Retry-After"))
	}
}

func TestUnpairedGets401(t *testing.T) {
	e := newEnv(t)
	e.paired()
	c := e.newClient()
	id := bookid.FromBytes([]byte("x"))
	for _, req := range []struct{ method, path string }{
		{"GET", "/v1/info"},
		{"GET", "/v1/devices"},
		{"DELETE", "/v1/devices/me"},
		{"DELETE", "/v1/devices/abc"},
		{"GET", "/v1/library/changes"},
		{"POST", "/v1/library/records"},
		{"PUT", "/v1/books/" + id + "/file"},
		{"GET", "/v1/books/" + id + "/file"},
	} {
		var body any
		if req.method == "POST" {
			body = map[string]any{"records": []any{}}
		}
		expect(t, c.do(req.method, req.path, body, nil), 401, "unknown_device")
	}
}

func rec(id string, page int, updatedAt int64) map[string]any {
	return map[string]any{
		"bookId": id, "fileName": "книга.pdf", "fileSize": 1000, "title": "Книга",
		"lastPage": page, "pageCount": 100, "progress": float64(page) / 100, "updatedAt": updatedAt, "deleted": false,
	}
}

func records(t *testing.T, r response) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, x := range r.json(t)["records"].([]any) {
		out = append(out, x.(map[string]any))
	}
	return out
}

func TestRecordsBetweenDevices(t *testing.T) {
	e := newEnv(t)
	a, b := e.paired(), e.paired()
	id1 := fmt.Sprintf("b1_%064x", 1)
	id2 := fmt.Sprintf("b1_%064x", 2)

	r := a.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(id1, 10, 1000), rec(id2, 0, 1000)}}, nil)
	expect(t, r, 200, "")
	got := records(t, r)
	if len(got) != 2 || got[0]["bookId"] != id1 || got[0]["rev"] != 1.0 || got[0]["updatedBy"] != a.id || got[0]["hasFile"] != false {
		t.Fatalf("push = %v", got)
	}

	ch := b.do("GET", "/v1/library/changes?since=0", nil, nil)
	expect(t, ch, 200, "")
	m := ch.json(t)
	if m["cursor"] != 2.0 || m["hasMore"] != false || len(m["records"].([]any)) != 2 {
		t.Fatalf("changes = %v", m)
	}

	// B reads further; a stale write from A afterwards loses.
	r = b.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(id1, 42, 2000)}}, nil)
	expect(t, r, 200, "")
	if got := records(t, r)[0]; got["lastPage"] != 42.0 || got["updatedBy"] != b.id || got["rev"] != 3.0 {
		t.Fatalf("newer = %v", got)
	}
	r = a.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(id1, 11, 1500)}}, nil)
	if got := records(t, r)[0]; got["lastPage"] != 42.0 || got["rev"] != 3.0 {
		t.Fatalf("stale write won: %v", got)
	}

	ch = a.do("GET", "/v1/library/changes?since=2", nil, nil)
	m = ch.json(t)
	if recs := m["records"].([]any); len(recs) != 1 || recs[0].(map[string]any)["lastPage"] != 42.0 || m["cursor"] != 3.0 {
		t.Fatalf("A changes = %v", m)
	}

	// Pagination.
	ch = a.do("GET", "/v1/library/changes?since=0&limit=1", nil, nil)
	m = ch.json(t)
	if m["hasMore"] != true || m["cursor"] != 2.0 {
		t.Fatalf("page = %v", m)
	}
	ch = a.do("GET", "/v1/library/changes?since=3", nil, nil)
	if m = ch.json(t); m["cursor"] != 3.0 || len(m["records"].([]any)) != 0 {
		t.Fatalf("empty page = %v", m)
	}

	// Validation; nothing from a rejected batch is stored.
	bad := []map[string]any{
		{"bookId": "nope", "fileName": "a", "fileSize": 1, "updatedAt": 1},
		{"bookId": id1, "fileName": "a/b", "fileSize": 1, "updatedAt": 1},
		{"bookId": id1, "fileName": `a\b`, "fileSize": 1, "updatedAt": 1},
		{"bookId": id1, "fileName": "a", "fileSize": 1, "updatedAt": 1, "progress": 1.5},
		{"bookId": id1, "fileName": "a", "fileSize": -1, "updatedAt": 1},
		{"bookId": id1, "fileName": "a", "fileSize": 1},
	}
	for _, x := range bad {
		third := fmt.Sprintf("b1_%064x", 3)
		r := a.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(third, 1, 1), x}}, nil)
		expect(t, r, 400, "bad_request")
	}
	many := make([]any, 501)
	for i := range many {
		many[i] = rec(fmt.Sprintf("b1_%064x", 100+i), 0, 1)
	}
	expect(t, a.do("POST", "/v1/library/records", map[string]any{"records": many}, nil), 400, "bad_request")
	expect(t, a.do("GET", "/v1/library/changes?limit=0", nil, nil), 400, "bad_request")
	expect(t, a.do("GET", "/v1/library/changes?since=-1", nil, nil), 400, "bad_request")
	if m = a.do("GET", "/v1/library/changes", nil, nil).json(t); m["cursor"] != 3.0 {
		t.Fatalf("a rejected batch was stored: %v", m)
	}

	// Body over 2 MB.
	huge := []byte(`{"records":[],"x":"` + strings.Repeat("a", server.MaxJSONBody) + `"}`)
	expect(t, a.do("POST", "/v1/library/records", huge, nil), 413, "too_large")
}

func book(n, mul int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(mul * i % 256)
	}
	return b
}

func TestFiles(t *testing.T) {
	e := newEnv(t, func(c *server.Config) { c.MaxFileSize = 1 << 20 })
	a, b := e.paired(), e.paired()
	data := book(300_000, 7)
	id := bookid.FromBytes(data)
	path := "/v1/books/" + id + "/file"

	expect(t, a.do("PUT", path, data, nil), 404, "not_found")
	expect(t, a.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(id, 0, 1)}}, nil), 200, "")

	expect(t, b.do("GET", path, nil, nil), 404, "not_found")
	expect(t, b.do("HEAD", path, nil, nil), 404, "")

	// Wrong content for this bookId.
	other := book(300_000, 3)
	expect(t, a.do("PUT", path, other, nil), 422, "book_id_mismatch")

	// No Content-Length: the body is chunked.
	expect(t, a.do("PUT", path, io.MultiReader(bytes.NewReader(data)), nil), 411, "length_required")

	// Over the size limit.
	big := book(2<<20, 1)
	bigID := bookid.FromBytes(big)
	a.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(bigID, 0, 1)}}, nil)
	expect(t, a.do("PUT", "/v1/books/"+bigID+"/file", big, nil), 413, "too_large")

	expect(t, a.do("PUT", path, data, nil), 204, "")
	ch := b.do("GET", "/v1/library/changes?since=1", nil, nil).json(t)
	recs := ch["records"].([]any)
	last := recs[len(recs)-1].(map[string]any)
	if last["bookId"] != id || last["hasFile"] != true {
		t.Fatalf("after upload: %v", ch)
	}
	rev := last["rev"]

	// Same file again: no new rev.
	expect(t, b.do("PUT", path, data, nil), 204, "")
	if ch := b.do("GET", "/v1/library/changes?since=0", nil, nil).json(t); ch["cursor"] != rev {
		t.Fatalf("re-upload changed rev: %v", ch["cursor"])
	}

	// A newer record keeps hasFile.
	r := b.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(id, 5, 2)}}, nil)
	if got := records(t, r)[0]; got["hasFile"] != true {
		t.Fatalf("hasFile lost: %v", got)
	}

	r = b.do("GET", path, nil, nil)
	expect(t, r, 200, "")
	if !bytes.Equal(r.body, data) || r.header.Get("ETag") != `"`+id+`"` {
		t.Fatalf("download: %d bytes, etag %q", len(r.body), r.header.Get("ETag"))
	}
	r = b.do("GET", path, nil, map[string]string{"Range": "bytes=1000-1999"})
	expect(t, r, 206, "")
	if !bytes.Equal(r.body, data[1000:2000]) {
		t.Fatal("range body mismatch")
	}
	r = b.do("HEAD", path, nil, nil)
	if r.status != 200 || r.header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("HEAD: %d %v", r.status, r.header)
	}
	r = b.do("GET", path, nil, map[string]string{"If-None-Match": `"` + id + `"`})
	if r.status != http.StatusNotModified {
		t.Fatalf("If-None-Match: %d", r.status)
	}

	// A small file uses the whole-content hash.
	small := book(1000, 1)
	smallID := bookid.FromBytes(small)
	a.do("POST", "/v1/library/records", map[string]any{"records": []any{rec(smallID, 0, 1)}}, nil)
	expect(t, a.do("PUT", "/v1/books/"+smallID+"/file", small, nil), 204, "")
	if r := b.do("GET", "/v1/books/"+smallID+"/file", nil, nil); !bytes.Equal(r.body, small) {
		t.Fatal("small file mismatch")
	}
}

func TestDevices(t *testing.T) {
	e := newEnv(t)
	a, b, c := e.paired(), e.paired(), e.paired()

	r := a.do("GET", "/v1/devices", nil, nil)
	expect(t, r, 200, "")
	devs := r.json(t)["devices"].([]any)
	if len(devs) != 3 {
		t.Fatalf("devices = %v", devs)
	}
	for _, d := range devs {
		d := d.(map[string]any)
		if (d["deviceId"] == a.id) != (d["current"] == true) {
			t.Fatalf("current flag wrong: %v", d)
		}
		if d["deviceId"] == a.id && (d["fingerprint"] != a.fp || d["lastSeenAt"] == nil) {
			t.Fatalf("device a: %v", d)
		}
	}

	expect(t, a.do("DELETE", "/v1/devices/"+b.id, nil, nil), 204, "")
	expect(t, b.do("GET", "/v1/info", nil, nil), 401, "unknown_device")
	expect(t, a.do("DELETE", "/v1/devices/"+b.id, nil, nil), 404, "not_found")

	expect(t, c.do("DELETE", "/v1/devices/me", nil, nil), 204, "")
	expect(t, c.do("GET", "/v1/info", nil, nil), 401, "unknown_device")
	// An unpaired device can pair again.
	expect(t, c.pairWith(e.newCode(time.Minute)), 200, "")
	expect(t, c.do("GET", "/v1/info", nil, nil), 200, "")
}
