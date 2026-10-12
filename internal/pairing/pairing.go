// SPDX-License-Identifier: AGPL-3.0-or-later

// Package pairing implements one-time pairing codes and the HMAC proofs
// exchanged during pairing (see api/openapi.yaml).
package pairing

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Alphabet is Crockford's base32 alphabet.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// CodeLength is the number of significant characters in a code (80 bits).
const CodeLength = 16

// NewCode returns a fresh normalized code of CodeLength characters.
func NewCode() (string, error) {
	buf := make([]byte, CodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, CodeLength)
	for i, b := range buf {
		// 256 is a multiple of 32, so b%32 is uniform.
		out[i] = Alphabet[b%32]
	}
	return string(out), nil
}

// Format shows a normalized code as XXXX-XXXX-XXXX-XXXX.
func Format(code string) string {
	var sb strings.Builder
	for i, r := range code {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// Normalize uppercases the code, drops spaces and dashes, and maps the
// look-alike letters O, I and L to digits.
func Normalize(code string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(code) {
		switch r {
		case ' ', '-', '\t':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// Proof returns hex(HMAC-SHA256(normalize(code),
// "bookmarks-pair-v1|<role>|<serverFP>|<clientFP>")).
func Proof(code, role, serverFP, clientFP string) string {
	mac := hmac.New(sha256.New, []byte(Normalize(code)))
	mac.Write([]byte("bookmarks-pair-v1|" + role + "|" + serverFP + "|" + clientFP))
	return hex.EncodeToString(mac.Sum(nil))
}

// ClientProof is Proof with role "client".
func ClientProof(code, serverFP, clientFP string) string {
	return Proof(code, "client", serverFP, clientFP)
}

// ServerProof is Proof with role "server".
func ServerProof(code, serverFP, clientFP string) string {
	return Proof(code, "server", serverFP, clientFP)
}

// Verify compares a hex proof sent by the client in constant time.
func Verify(got, want string) bool {
	return hmac.Equal([]byte(got), []byte(want))
}
