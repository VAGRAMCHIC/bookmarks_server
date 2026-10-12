// SPDX-License-Identifier: AGPL-3.0-or-later

package certs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreate(t *testing.T) {
	dir := t.TempDir()
	c1, created, err := LoadOrCreate(dir, "Дом")
	if err != nil || !created {
		t.Fatalf("first: created=%v err=%v", created, err)
	}
	if c1.Leaf.Subject.CommonName != "Дом" {
		t.Errorf("CN = %q", c1.Leaf.Subject.CommonName)
	}
	st, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key perms: %v %v", st.Mode(), err)
	}
	c2, created, err := LoadOrCreate(dir, "Другое")
	if err != nil || created {
		t.Fatalf("second: created=%v err=%v", created, err)
	}
	fp1 := Fingerprint(c1.Certificate[0])
	if Fingerprint(c2.Certificate[0]) != fp1 {
		t.Fatal("certificate changed on reload")
	}
	if fp, err := LoadFingerprint(dir); err != nil || fp != fp1 {
		t.Fatalf("LoadFingerprint = %s, %v", fp, err)
	}
	if len(fp1) != 64 {
		t.Fatalf("fingerprint %q", fp1)
	}
}

func TestGroup(t *testing.T) {
	if got := Group("ab12cd34ef"); got != "AB12 CD34 EF" {
		t.Fatalf("Group = %q", got)
	}
}
