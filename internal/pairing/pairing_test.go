// SPDX-License-Identifier: AGPL-3.0-or-later

package pairing

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	if got := Normalize("abcd-efgh ijkl-mnop"); got != "ABCDEFGH1JK1MN0P" {
		t.Fatalf("Normalize = %q", got)
	}
}

func TestProofs(t *testing.T) {
	code := "abcd-efgh-jkmn-pqrs"
	s := strings.Repeat("11", 32)
	c := strings.Repeat("22", 32)
	if got := ClientProof(code, s, c); got != "66d08b9159e835d1821ae1eccf1339febd44ca6a07be1b6b1cb9691beb62177e" {
		t.Errorf("ClientProof = %s", got)
	}
	if got := ServerProof(code, s, c); got != "b4975d23088e6fbc4b83f6ef93d5810353dcbc833a4b453dddb0eba9bcde120b" {
		t.Errorf("ServerProof = %s", got)
	}
}

func TestNewCode(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		code, err := NewCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != CodeLength || Normalize(code) != code {
			t.Fatalf("bad code %q", code)
		}
		for _, r := range code {
			if !strings.ContainsRune(Alphabet, r) {
				t.Fatalf("bad char in %q", code)
			}
		}
		if seen[code] {
			t.Fatalf("duplicate code %q", code)
		}
		seen[code] = true
	}
	if got := Format("ABCDEFGHJKMNPQRS"); got != "ABCD-EFGH-JKMN-PQRS" {
		t.Fatalf("Format = %q", got)
	}
}
