package llama

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nogo/faden"
)

const (
	readName  = "read"
	writeName = "write"
	editName  = "edit"

	// maxEditBytes bounds the file edit holds in memory.
	maxEditBytes = 1 << 20
)

var fileToolNames = []string{readName, writeName, editName}

// confineScript resolves "$2" and fails unless it stays inside the directory
// "$1" after symlinks. The path may not exist yet: its deepest existing
// ancestor is resolved instead. Every file script starts with it. Exit 4
// means the path escapes.
const confineScript = `root=$(readlink -f "$1") || exit 3
f=$2
d=$f
while [ ! -e "$d" ] && [ ! -L "$d" ]; do d=$(dirname "$d"); done
real=$(readlink -f "$d") || exit 3
case "$real/" in "$root"/*) ;; *) echo "path is outside the working directory" >&2; exit 4;; esac
`

// binaryCheck fails with exit 5 when the first 8 KiB of "$f" hold a NUL byte.
const binaryCheck = `[ -f "$f" ] || { echo "not a file" >&2; exit 6; }
[ "$(head -c 8192 "$f" | tr -d '\000' | wc -c)" -eq "$(head -c 8192 "$f" | wc -c)" ] || { echo "binary file" >&2; exit 5; }
`

const readScript = confineScript + binaryCheck + `tail -n +"$3" "$f" | { if [ "$4" -gt 0 ]; then head -n "$4"; else cat; fi; } | head -c "$5"`

const writeScript = confineScript + `mkdir -p "$(dirname "$f")" && cat > "$f"`

const editReadScript = confineScript + binaryCheck + `[ "$(wc -c < "$f")" -le "$3" ] || { echo "file is larger than $3 bytes" >&2; exit 7; }
cat "$f"`

// fileTools returns read, write and edit, which run through rt in the
// attempt's Dir. maxOutput bounds what read returns.
func fileTools(rt faden.Runtime, key string, setup faden.Setup, maxOutput int) []Tool {
	f := fileRunner{rt: rt, key: key, setup: setup}
	return []Tool{
		{
			Name:        readName,
			Description: "Read a text file. Paths are relative to the working directory. Lines start at 1; offset and limit select a range, limit 0 reads to the end.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["path"]}`),
			Safe:        true,
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Path   string `json:"path"`
					Offset int    `json:"offset"`
					Limit  int    `json:"limit"`
				}
				if err := json.Unmarshal(args, &a); err != nil {
					return "", errors.New("arguments need a path, and offset and limit as integers")
				}
				if a.Offset < 1 {
					a.Offset = 1
				}
				if a.Limit < 0 {
					a.Limit = 0
				}
				// One byte over the limit lets the loop's cut mark the result.
				out, err := f.run(ctx, a.Path, nil, readScript, fmt.Sprint(a.Offset), fmt.Sprint(a.Limit), fmt.Sprint(maxOutput+1))
				return string(out), err
			},
		},
		{
			Name:        writeName,
			Description: "Write a file, replacing it, and create missing directories. Paths are relative to the working directory.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Path    string  `json:"path"`
					Content *string `json:"content"`
				}
				if err := json.Unmarshal(args, &a); err != nil || a.Content == nil {
					return "", errors.New("arguments need a path and content")
				}
				if _, err := f.run(ctx, a.Path, strings.NewReader(*a.Content), writeScript); err != nil {
					return "", err
				}
				return fmt.Sprintf("wrote %d bytes to %s", len(*a.Content), a.Path), nil
			},
		},
		{
			Name:        editName,
			Description: "Replace text in a file. old must match exactly once. Paths are relative to the working directory.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]}`),
			Run: func(ctx context.Context, args json.RawMessage) (string, error) {
				var a struct {
					Path string  `json:"path"`
					Old  string  `json:"old"`
					New  *string `json:"new"`
				}
				if err := json.Unmarshal(args, &a); err != nil || a.Old == "" || a.New == nil {
					return "", errors.New("arguments need a path, a non-empty old and new")
				}
				text, err := f.run(ctx, a.Path, nil, editReadScript, fmt.Sprint(maxEditBytes))
				if err != nil {
					return "", err
				}
				if !utf8.Valid(text) {
					return "", errors.New("file is not valid UTF-8")
				}
				switch n := strings.Count(string(text), a.Old); n {
				case 1:
				case 0:
					return "", errors.New("old text not found")
				default:
					return "", fmt.Errorf("old text matches %d times; it must match once", n)
				}
				edited := strings.Replace(string(text), a.Old, *a.New, 1)
				if _, err := f.run(ctx, a.Path, strings.NewReader(edited), writeScript); err != nil {
					return "", err
				}
				return "edited " + a.Path, nil
			},
		},
	}
}

type fileRunner struct {
	rt    faden.Runtime
	key   string
	setup faden.Setup
}

// run executes a script that starts with confineScript. extra become $3 on.
func (f fileRunner) run(ctx context.Context, path string, stdin *strings.Reader, script string, extra ...string) ([]byte, error) {
	dir := f.setup.Dir
	if dir == "" {
		dir = "."
	}
	target, err := lexicalPath(dir, path)
	if err != nil {
		return nil, err
	}
	argv := append([]string{"sh", "-c", script, "sh", dir, target}, extra...)
	cmd := faden.Command{Key: f.key, Dir: f.setup.Dir, Env: f.setup.Env, Argv: argv}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	res, err := f.rt.Run(ctx, cmd)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("%s[exit code %d]", res.Output, res.ExitCode)
	}
	return res.Output, nil
}

// lexicalPath joins path to dir and removes "." and ".." without touching
// the file system. The shell script then checks symlinks.
func lexicalPath(dir, path string) (string, error) {
	if path == "" {
		return "", errors.New("path is empty")
	}
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	p = filepath.Clean(p)
	if filepath.IsAbs(dir) || filepath.IsAbs(p) {
		if filepath.IsAbs(dir) {
			if rel, err := filepath.Rel(filepath.Clean(dir), p); err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return "", errors.New("path is outside the working directory")
			}
		}
		return p, nil
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return "", errors.New("path is outside the working directory")
	}
	return p, nil
}
