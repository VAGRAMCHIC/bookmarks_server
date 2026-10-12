// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vagramchic/bookmarks-server/internal/pairing"
	"github.com/vagramchic/bookmarks-server/internal/store"
)

func runCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(context.Background(), args, &out, &errOut)
	return out.String() + errOut.String(), err
}

func TestCommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")

	if _, err := runCmd(t, "pair", "--data", dir); err == nil {
		t.Fatal("pair without a certificate succeeded")
	}

	// serve creates the certificate and stops on cancel.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve", "--data", dir, "--listen", addr, "--name", "Дом"}, &out, &bytes.Buffer{}) }()
	var conn *tls.Conn
	for range 100 {
		conn, err = tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		cancel()
		t.Fatalf("server did not start: %v", err)
	}
	cn := conn.ConnectionState().PeerCertificates[0].Subject.CommonName
	conn.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if cn != "Дом" || !strings.Contains(out.String(), "Создан новый сертификат") {
		t.Fatalf("cn %q, output %q", cn, out.String())
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode: %v %v", st.Mode(), err)
	}

	fp, err := runCmd(t, "fingerprint", "--data", dir)
	if err != nil || !regexp.MustCompile(`^([0-9A-F]{4} ){15}[0-9A-F]{4}\n$`).MatchString(fp) {
		t.Fatalf("fingerprint: %q %v", fp, err)
	}

	pairOut, err := runCmd(t, "pair", "--data", dir, "--ttl", "5m")
	if err != nil || !regexp.MustCompile(`[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}-[0-9A-Z]{4}`).MatchString(pairOut) ||
		!strings.Contains(pairOut, strings.TrimSpace(fp)) || !strings.Contains(pairOut, "Синхронизация → Сопрячь") {
		t.Fatalf("pair: %q %v", pairOut, err)
	}

	if out, err := runCmd(t, "devices", "--data", dir); err != nil || !strings.Contains(out, "нет") {
		t.Fatalf("devices: %q %v", out, err)
	}
	// With a full device list pair refuses to issue a code.
	st, err := openStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := pairing.NewCode()
	st.SetPairingCode(context.Background(), code, time.Now().Add(time.Minute))
	_, _, err = st.Pair(context.Background(), store.PairRequest{Name: "x", Platform: "linux",
		ServerFingerprint: "s", ClientFingerprint: "c", ClientProof: pairing.ClientProof(code, "s", "c"), Now: time.Now()})
	st.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOOKMARKS_MAX_DEVICES", "1")
	if out, err := runCmd(t, "pair", "--data", dir); err == nil || !strings.Contains(err.Error(), "1 из 1") {
		t.Fatalf("pair over the limit: %q %v", out, err)
	}
	if _, err := runCmd(t, "pair", "--data", dir, "--max-devices", "2"); err != nil {
		t.Fatalf("pair under the limit: %v", err)
	}

	if _, err := runCmd(t, "remove", "--data", dir, "nope"); err == nil {
		t.Fatal("remove of unknown device succeeded")
	}
	if _, err := runCmd(t, "bogus"); err == nil {
		t.Fatal("unknown command succeeded")
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"500M": 500 << 20, "1g": 1 << 30, "10KiB": 10 << 10, "123": 123, "2MB": 2 << 20} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "-1M", "0"} {
		if _, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) succeeded", in)
		}
	}
}
