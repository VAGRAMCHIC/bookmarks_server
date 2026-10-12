// SPDX-License-Identifier: AGPL-3.0-or-later

// Package certs creates and loads the server's self-signed certificate and
// computes certificate fingerprints.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	KeyFile  = "server.key"
	CertFile = "server.crt"
)

// Fingerprint is hex(sha256(der)) in lower case.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// Group shows a fingerprint as the app does: upper case, groups of 4.
func Group(fp string) string {
	fp = strings.ToUpper(fp)
	var parts []string
	for i := 0; i < len(fp); i += 4 {
		parts = append(parts, fp[i:min(i+4, len(fp))])
	}
	return strings.Join(parts, " ")
}

// Generate creates an ECDSA P-256 key and a self-signed certificate valid
// for ten years, starting a day in the past.
func Generate(commonName string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-24 * time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// LoadOrCreate loads server.key and server.crt from dir, creating them if
// neither exists. created reports whether new files were written.
func LoadOrCreate(dir, commonName string) (cert tls.Certificate, created bool, err error) {
	keyPath := filepath.Join(dir, KeyFile)
	certPath := filepath.Join(dir, CertFile)
	cert, err = Load(dir)
	if err == nil {
		return cert, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return cert, false, err
	}
	if _, kerr := os.Stat(keyPath); kerr == nil {
		return cert, false, fmt.Errorf("найден %s без %s: восстановите сертификат из резервной копии", KeyFile, CertFile)
	}
	cert, err = Generate(commonName)
	if err != nil {
		return cert, false, err
	}
	keyDER, err := x509.MarshalECPrivateKey(cert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return cert, false, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return cert, false, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return cert, false, err
	}
	return cert, true, nil
}

// Load reads server.key and server.crt from dir.
func Load(dir string) (tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, CertFile), filepath.Join(dir, KeyFile))
	if err != nil {
		return cert, err
	}
	return cert, nil
}

// LoadFingerprint returns the fingerprint of server.crt in dir.
func LoadFingerprint(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, CertFile))
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s: не PEM-сертификат", CertFile)
	}
	return Fingerprint(block.Bytes), nil
}
