package attach

import (
	"encoding/base64"
	"os"
	"path/filepath"
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
