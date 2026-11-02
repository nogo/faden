// Package attach prepares the files of a turn for a backend. A host names
// files by path; a backend sends an image as an image and any other file as a
// path line. Use Open to sort the files, Lines for the text a model reads.
package attach

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// MaxImageBytes is the largest file sent as an image. A larger one goes as a path line.
const MaxImageBytes = 10 << 20

// Image is an open image file. Its bytes stay on disk until Write or a backend reads them.
type Image struct {
	// Path is the path the host gave, relative to the attempt's directory.
	Path string
	// Abs is the absolute path of the file.
	Abs string
	// MIME is the media type found from the file's bytes.
	MIME string
	file *os.File
}

// Set is the files of one turn, sorted into images and the rest.
type Set struct {
	Images []Image
	// Others are the paths of files sent as path lines.
	Others []string
}

// Lines returns one "Attached file: <path>" line for each path, each ended by a newline.
func Lines(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("Attached file: " + p + "\n")
	}
	return b.String()
}

// WithLines returns text followed by the path lines of paths.
func WithLines(text string, paths []string) string {
	if len(paths) == 0 {
		return text
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + Lines(paths)
}

// Open sorts the files named by paths, relative to dir. An image is PNG, JPEG,
// GIF or WebP by its first bytes, at most MaxImageBytes. A path outside dir or
// a file that cannot be read is an error. Close the Set when done.
func Open(dir string, paths []string) (*Set, error) {
	s := &Set{}
	for _, p := range paths {
		if !filepath.IsLocal(p) {
			s.Close()
			return nil, fmt.Errorf("attach: %q is outside the working directory", p)
		}
		abs, err := filepath.Abs(filepath.Join(dir, p))
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("attach: %q: %w", p, err)
		}
		f, err := os.Open(abs)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("attach: %w", err)
		}
		mime, err := sniff(f)
		if err != nil {
			f.Close()
			s.Close()
			return nil, fmt.Errorf("attach: %q: %w", p, err)
		}
		if mime == "" {
			f.Close()
			s.Others = append(s.Others, p)
			continue
		}
		s.Images = append(s.Images, Image{Path: p, Abs: abs, MIME: mime, file: f})
	}
	return s, nil
}

// sniff returns the image type of f, or "" for any other file or an image over the limit.
func sniff(f *os.File) (string, error) {
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	if info.Size() > MaxImageBytes {
		return "", nil
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	switch mime := http.DetectContentType(head[:n]); mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mime, nil
	}
	return "", nil
}

// Close closes the open image files.
func (s *Set) Close() {
	for _, im := range s.Images {
		im.file.Close()
	}
}

// writeBase64 streams the image as base64 into w, so the file is never held whole.
func (im Image) writeBase64(w io.Writer) error {
	enc := base64.NewEncoder(base64.StdEncoding, w)
	if _, err := io.Copy(enc, io.LimitReader(im.file, MaxImageBytes)); err != nil {
		return err
	}
	return enc.Close()
}
