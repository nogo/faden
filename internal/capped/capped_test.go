package capped

import (
	"strings"
	"testing"
)

func write(b *Buffer, parts ...string) {
	for _, s := range parts {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			panic("short write")
		}
	}
}

func TestBufferKeepsStartAndEndAndCountsTheRest(t *testing.T) {
	b := &Buffer{Max: 9} // 3 from the start, 6 from the end
	write(b, "abc", "defg", "hij", "klmnop")
	if got, want := string(b.Bytes()), "abc\n[7 bytes left out]\nklmnop"; got != want {
		t.Fatalf("kept %q, want %q", got, want)
	}
}

func TestBufferThatFitsHasNoMarker(t *testing.T) {
	b := &Buffer{Max: 9}
	write(b, "abcd", "efghi")
	if got := string(b.Bytes()); got != "abcdefghi" {
		t.Fatalf("kept %q", got)
	}
}

func TestBufferBoundsOneHugeWrite(t *testing.T) {
	b := &Buffer{Max: 30}
	write(b, strings.Repeat("x", 1<<20)+"END")
	if len(b.tail) > 2*20 {
		t.Fatalf("tail holds %d bytes", len(b.tail))
	}
	if got := string(b.Bytes()); !strings.HasPrefix(got, "xxxxxxxxxx\n[") || !strings.HasSuffix(got, "xEND") {
		t.Fatalf("kept %q", got)
	}
}

func TestBufferTailStaysBoundedOverManyWrites(t *testing.T) {
	b := &Buffer{Max: 30}
	for range 10000 {
		write(b, "0123456789")
	}
	if len(b.tail) > 2*20 || cap(b.tail) > 4*20 {
		t.Fatalf("tail len %d cap %d", len(b.tail), cap(b.tail))
	}
	if got := string(b.Bytes()); !strings.HasSuffix(got, "]\n01234567890123456789") {
		t.Fatalf("kept %q", got)
	}
}

func TestBufferCutsBetweenRunes(t *testing.T) {
	b := &Buffer{Max: 9}                          // 3 from the start, 6 from the end
	write(b, "éé"+strings.Repeat("-", 20)+"üöäb") // both cuts fall inside a rune
	got := string(b.Bytes())
	if want := "é\n[24 bytes left out]\nöäb"; got != want {
		t.Fatalf("kept %q, want %q", got, want)
	}
}

func TestBufferWithoutMaxKeepsEverything(t *testing.T) {
	b := &Buffer{}
	s := strings.Repeat("x", 100000)
	write(b, s)
	if string(b.Bytes()) != s {
		t.Fatalf("kept %d bytes, want %d", len(b.Bytes()), len(s))
	}
}
