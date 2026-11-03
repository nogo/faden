package claude

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nogo/faden/internal/attach"
)

// tinyPNG is an invented 1x1 PNG.
var tinyPNG, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==")

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestUserMessageHoldsTextAndImageAsBase64(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a.png", tinyPNG)
	s, err := attach.Open(dir, []string{"a.png"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, stop := userMessage("say \"hi\"", s.Images)
	data, err := io.ReadAll(r)
	stop()
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Message struct {
			Content []struct {
				Type, Text string
				Source     struct{ Media_type, Data string }
			}
		}
	}
	if err := json.Unmarshal(data, &m); err != nil || !bytes.HasSuffix(data, []byte("\n")) {
		t.Fatalf("%v: %s", err, data)
	}
	c := m.Message.Content
	got, _ := base64.StdEncoding.DecodeString(c[1].Source.Data)
	if len(c) != 2 || c[0].Text != `say "hi"` || c[1].Type != "image" || c[1].Source.Media_type != "image/png" || !bytes.Equal(got, tinyPNG) {
		t.Fatalf("content = %+v", c)
	}
}

// TestImageBytesAreNotHeldWhileSending guards the memory rule: a 9 MB image
// is encoded in chunks, so the send allocates far less than the file.
func TestImageBytesAreNotHeldWhileSending(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "big.png", append(append([]byte(nil), tinyPNG...), make([]byte, 9<<20)...))
	s, err := attach.Open(dir, []string{"big.png"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r, stop := userMessage("x", s.Images)
	n, err := io.Copy(io.Discard, r)
	stop()
	runtime.ReadMemStats(&after)
	if err != nil || n < 12<<20 {
		t.Fatalf("sent %d bytes, %v", n, err)
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Fatalf("send allocated %d bytes for a 9 MB image", got)
	}
}
