// Package capped is a buffer that keeps the start and the end of what is
// written to it and drops the middle. Use it where a child process writes
// output of unknown size: a command's errors and summary come last, so the
// end matters as much as the start.
package capped

import (
	"fmt"
	"unicode/utf8"
)

// Buffer keeps at most Max bytes: the first third and the last two thirds of
// what was written. Max zero keeps everything. Write never fails and always
// reports the whole write, so a process writing to it is drained to its end.
type Buffer struct {
	Max   int
	head  []byte
	tail  []byte // grows to twice the tail size, then keeps its last tail size
	total int64
}

// Write implements io.Writer.
func (b *Buffer) Write(p []byte) (int, error) {
	n := len(p)
	b.total += int64(n)
	if b.Max <= 0 {
		b.head = append(b.head, p...)
		return n, nil
	}
	headMax, tailMax := b.sizes()
	if room := headMax - len(b.head); room > 0 {
		k := min(room, len(p))
		b.head = append(b.head, p[:k]...)
		p = p[k:]
	}
	if len(p) >= tailMax {
		b.tail = append(b.tail[:0], p[len(p)-tailMax:]...)
		return n, nil
	}
	b.tail = append(b.tail, p...)
	if len(b.tail) > 2*tailMax {
		b.tail = append(b.tail[:0], b.tail[len(b.tail)-tailMax:]...)
	}
	return n, nil
}

// Bytes returns what the buffer kept. When it dropped bytes, a marker line
// between the start and the end says how many; the cuts fall between runes.
func (b *Buffer) Bytes() []byte {
	head, tail := b.head, b.tail
	if b.Max > 0 {
		_, tailMax := b.sizes()
		tail = tail[max(len(tail)-tailMax, 0):]
	}
	if b.total == int64(len(head)+len(tail)) {
		return append(head[:len(head):len(head)], tail...)
	}
	head = head[:runeEnd(head)]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	left := b.total - int64(len(head)+len(tail))
	out := make([]byte, 0, len(head)+len(tail)+40)
	out = append(out, head...)
	out = fmt.Appendf(out, "\n[%d bytes left out]\n", left)
	return append(out, tail...)
}

// sizes splits Max into the bytes kept from the start and from the end.
func (b *Buffer) sizes() (head, tail int) {
	head = b.Max / 3
	return head, b.Max - head
}

// runeEnd is the length of p without a rune that the cut split.
func runeEnd(p []byte) int {
	for i := len(p) - 1; i >= max(len(p)-utf8.UTFMax, 0); i-- {
		if utf8.RuneStart(p[i]) {
			if !utf8.FullRune(p[i:]) {
				return i
			}
			break
		}
	}
	return len(p)
}
