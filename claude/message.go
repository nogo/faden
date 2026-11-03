package claude

import (
	"bytes"
	"encoding/json"
	"io"

	"github.com/nogo/faden/internal/attach"
)

// userMessage returns Claude's stream-json user message on one line: the
// text alone, or the text followed by the images as base64 blocks. An image
// is encoded while the reader is read, so no image is held whole. Call stop
// once the reader is no longer needed; it waits for the encoder, after which
// the image files may be closed.
func userMessage(text string, images []attach.Image) (r io.Reader, stop func()) {
	if len(images) == 0 {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": text}}) // strings cannot fail
		return bytes.NewReader(append(line, '\n')), func() {}
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		writer.CloseWithError(writeUserMessage(writer, text, images))
	}()
	return reader, func() { reader.Close(); <-done }
}

func writeUserMessage(w io.Writer, text string, images []attach.Image) error {
	quoted, _ := json.Marshal(text) // a string cannot fail
	if _, err := io.WriteString(w, `{"type":"user","message":{"role":"user","content":[{"type":"text","text":`+string(quoted)+`}`); err != nil {
		return err
	}
	for _, im := range images {
		if _, err := io.WriteString(w, `,{"type":"image","source":{"type":"base64","media_type":"`+im.MIME+`","data":"`); err != nil {
			return err
		}
		if err := im.WriteBase64(w); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `"}}`); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]}}\n")
	return err
}
