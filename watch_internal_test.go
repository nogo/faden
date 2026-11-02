package faden

import (
	"strings"
	"testing"
	"unsafe"
)

func TestWatcherFragmentBound(t *testing.T) {
	w := &watcher{session: "s", fragments: make(chan Fragment, fragmentSlots)}
	r := &Runner{watchers: map[*watcher]struct{}{w: {}}}
	for range 1000 {
		r.fragment("s", Fragment{RequestID: "r", Kind: FragmentText, Text: strings.Repeat("x", fragmentBytes)})
	}
	if len(w.fragments) != fragmentSlots {
		t.Fatalf("queue length %d", len(w.fragments))
	}
	r.fragment("s", Fragment{Text: strings.Repeat("x", fragmentBytes+1)})
	for range fragmentSlots {
		f := <-w.fragments
		if len(f.Text) > fragmentBytes {
			t.Fatal("oversized fragment retained")
		}
	}
	r.fragment("s", Fragment{Text: strings.Repeat("x", fragmentBytes+1)})
	if len(w.fragments) != 0 {
		t.Fatal("oversized fragment accepted")
	}
	t.Logf("fragment payload bound: %d bytes; queue values: %d bytes", fragmentSlots*fragmentBytes, fragmentSlots*int(unsafe.Sizeof(Fragment{})))
}
