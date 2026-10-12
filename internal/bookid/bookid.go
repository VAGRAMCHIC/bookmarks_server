// SPDX-License-Identifier: AGPL-3.0-or-later

// Package bookid computes the content-based book identifier:
//
//	"b1_" + hex(SHA-256(ascii(size) + "\n" + data))                 size <= 128 KiB
//	"b1_" + hex(SHA-256(ascii(size) + "\n" + first64K + last64K))   otherwise
package bookid

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"regexp"
	"strconv"
)

// Chunk is the size of the head and tail hashed for large files.
const Chunk = 64 * 1024

var pattern = regexp.MustCompile(`^b1_[0-9a-f]{64}$`)

// Valid reports whether id looks like a bookId.
func Valid(id string) bool { return pattern.MatchString(id) }

// FromBytes computes the bookId of an in-memory file.
func FromBytes(data []byte) string {
	h := sha256.New()
	h.Write([]byte(strconv.Itoa(len(data)) + "\n"))
	if len(data) <= 2*Chunk {
		h.Write(data)
	} else {
		h.Write(data[:Chunk])
		h.Write(data[len(data)-Chunk:])
	}
	return "b1_" + hex.EncodeToString(h.Sum(nil))
}

// Writer computes the bookId of a stream of known size while it is written,
// keeping only the head and a ring buffer of the tail in memory.
type Writer struct {
	size    int64
	written int64
	head    []byte
	tail    []byte // ring buffer of the last Chunk bytes
	tailPos int
}

// NewWriter returns a Writer for a stream of exactly size bytes.
func NewWriter(size int64) *Writer {
	return &Writer{size: size, head: make([]byte, 0, min(size, 2*Chunk))}
}

func (w *Writer) Write(p []byte) (int, error) {
	n := len(p)
	w.written += int64(n)
	if room := 2*Chunk - len(w.head); room > 0 {
		k := min(room, len(p))
		w.head = append(w.head, p[:k]...)
		p = p[k:]
	}
	// Bytes past the first 128 KiB go to the tail ring.
	for len(p) > 0 {
		if w.tail == nil {
			w.tail = make([]byte, Chunk)
		}
		k := copy(w.tail[w.tailPos:], p)
		w.tailPos = (w.tailPos + k) % Chunk
		p = p[k:]
	}
	return n, nil
}

// Written is the number of bytes seen so far.
func (w *Writer) Written() int64 { return w.written }

// Sum returns the bookId of everything written. It is meaningful only when
// Written() equals the declared size.
func (w *Writer) Sum() string {
	h := sha256.New()
	h.Write([]byte(strconv.FormatInt(w.written, 10) + "\n"))
	if w.written <= 2*Chunk {
		h.Write(w.head)
		return "b1_" + hex.EncodeToString(h.Sum(nil))
	}
	h.Write(w.head[:Chunk])
	// The last Chunk bytes are the tail of head plus everything in the ring.
	overflow := w.written - 2*Chunk // bytes stored in the ring, total
	if overflow >= Chunk {
		h.Write(w.tail[w.tailPos:])
		h.Write(w.tail[:w.tailPos])
	} else {
		k := int(overflow)
		h.Write(w.head[2*Chunk-(Chunk-k):])
		h.Write(w.tail[:k])
	}
	return "b1_" + hex.EncodeToString(h.Sum(nil))
}

// FromReader computes the bookId of r.
func FromReader(r io.Reader) (string, error) {
	w := NewWriter(0)
	if _, err := io.Copy(w, r); err != nil {
		return "", err
	}
	return w.Sum(), nil
}
