package attach

import (
	"encoding/json"
	"io"
)

// UserMessage returns a reader of one Claude stream-json user message holding
// text and the images as base64 blocks. A goroutine encodes each file while
// the reader is read, so no image is held whole. Call stop when the reader is
// no longer needed; it waits for the goroutine, after which the files may be closed.
func UserMessage(text string, images []Image) (r io.Reader, stop func()) {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		writer.CloseWithError(writeUserMessage(writer, text, images))
	}()
	return reader, func() { reader.Close(); <-done }
}

func writeUserMessage(w io.Writer, text string, images []Image) error {
	quoted, _ := json.Marshal(text) // a string cannot fail
	if _, err := io.WriteString(w, `{"type":"user","message":{"role":"user","content":[{"type":"text","text":`+string(quoted)+`}`); err != nil {
		return err
	}
	for _, im := range images {
		if _, err := io.WriteString(w, `,{"type":"image","source":{"type":"base64","media_type":"`+im.MIME+`","data":"`); err != nil {
			return err
		}
		if err := im.writeBase64(w); err != nil {
			return err
		}
		if _, err := io.WriteString(w, `"}}`); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, "]}}\n")
	return err
}
