package attach

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// png is an invented 1x1 PNG.
var png, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR4nGP4z8DwHwAFAAH/iZk9HQAAAABJRU5ErkJggg==")

func write(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenSortsByBytesNotName(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "picture.txt", png)
	write(t, dir, "notes.png", []byte("plain words"))
	write(t, dir, "big.png", append(append([]byte(nil), png...), make([]byte, MaxImageBytes)...))
	s, err := Open(dir, []string{"picture.txt", "notes.png", "big.png"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Images) != 1 || s.Images[0].Path != "picture.txt" || s.Images[0].MIME != "image/png" {
		t.Fatalf("images = %+v", s.Images)
	}
	if strings.Join(s.Others, ",") != "notes.png,big.png" {
		t.Fatalf("others = %v", s.Others)
	}
}

func TestOpenRefusesPathOutsideDirAndMissingFile(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"../x", "/etc/passwd", "missing.png"} {
		if s, err := Open(dir, []string{p}); err == nil {
			s.Close()
			t.Errorf("Open(%q) succeeded", p)
		}
	}
}

func TestUserMessageHoldsTextAndImageAsBase64(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.png", png)
	s, err := Open(dir, []string{"a.png"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, stop := UserMessage("say \"hi\"", s.Images)
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
	if len(c) != 2 || c[0].Text != `say "hi"` || c[1].Type != "image" || c[1].Source.Media_type != "image/png" || !bytes.Equal(got, png) {
		t.Fatalf("content = %+v", c)
	}
}

// TestImageBytesAreNotHeldWhileSending guards the memory rule: a 9 MB image
// is encoded in chunks, so the send allocates far less than the file.
func TestImageBytesAreNotHeldWhileSending(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "big.png", append(append([]byte(nil), png...), make([]byte, 9<<20)...))
	s, err := Open(dir, []string{"big.png"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r, stop := UserMessage("x", s.Images)
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
