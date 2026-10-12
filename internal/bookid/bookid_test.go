// SPDX-License-Identifier: AGPL-3.0-or-later

package bookid

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"
)

func gen(n, mul int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(mul * i % 256)
	}
	return b
}

func TestReferenceValues(t *testing.T) {
	cases := []struct {
		data []byte
		want string
	}{
		{gen(1000, 1), "b1_cd8d766a9055d784aab18bdf6e27634a0312036847c1894afbad7fc63e6ce12d"},
		{gen(200000, 7), "b1_4ccf5de6eac87b6fad17dd1c45c5401341a4240a9ebb808aae5ec641b022478b"},
	}
	for _, c := range cases {
		if got := FromBytes(c.data); got != c.want {
			t.Errorf("FromBytes(%d) = %s", len(c.data), got)
		}
		got, err := FromReader(iotest.OneByteReader(bytes.NewReader(c.data)))
		if err != nil || got != c.want {
			t.Errorf("FromReader(%d) = %s, %v", len(c.data), got, err)
		}
		if !Valid(c.want) {
			t.Errorf("Valid(%s) = false", c.want)
		}
	}
}

// The streaming hash must match the in-memory one around every boundary.
func TestWriterMatchesBytes(t *testing.T) {
	for _, n := range []int{0, 1, Chunk - 1, Chunk, 2*Chunk - 1, 2 * Chunk, 2*Chunk + 1,
		2*Chunk + Chunk/2, 3*Chunk - 1, 3 * Chunk, 3*Chunk + 1, 5*Chunk + 123} {
		data := gen(n, 13)
		for _, step := range []int{1, 1000, 70000, n + 1} {
			if step == 0 {
				continue
			}
			w := NewWriter(int64(n))
			for off := 0; off < n; off += step {
				w.Write(data[off:min(off+step, n)])
			}
			if got, want := w.Sum(), FromBytes(data); got != want {
				t.Fatalf("n=%d step=%d: %s != %s", n, step, got, want)
			}
		}
	}
	if _, err := FromReader(io.LimitReader(bytes.NewReader(nil), 0)); err != nil {
		t.Fatal(err)
	}
}
