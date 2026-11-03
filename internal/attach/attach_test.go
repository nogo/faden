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

func TestOpenSymlinks(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "work")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, parent, "outside.png", png)
	write(t, dir, "target.png", png)
	for _, name := range []string{"outside", "inside"} {
		target := filepath.Join(dir, "target.png")
		if name == "outside" {
			target = filepath.Join(parent, "outside.png")
		}
		if err := os.Symlink(target, filepath.Join(dir, name+".png")); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"outside", "inside"} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(dir, []string{name + ".png"})
			if s != nil {
				defer s.Close()
			}
			if name == "outside" {
				if err == nil || !strings.Contains(err.Error(), "outside the working directory") {
					t.Fatalf("Open outside symlink: set = %+v, error = %v", s, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Images) != 1 || s.Images[0].Abs != filepath.Join(dir, "target.png") {
				t.Fatalf("inside symlink images = %+v", s.Images)
			}
			var sent strings.Builder
			if err := s.Images[0].WriteBase64(&sent); err != nil {
				t.Fatal(err)
			}
			if sent.String() != base64.StdEncoding.EncodeToString(png) {
				t.Fatal("inside symlink sent different bytes")
			}
		})
	}
}
