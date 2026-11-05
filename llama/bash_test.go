package llama

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nogo/faden"
)

func TestBashKeepsLastWordsAndExitCodeOfLongOutput(t *testing.T) {
	dir := t.TempDir()
	const maxOutput = 300
	tool := bashTool(faden.Local{PIDDir: dir, Dir: dir}, "k", faden.Setup{}, maxOutput)
	_, err := tool.Run(t.Context(), json.RawMessage(`{"command":"head -c 100000 /dev/zero | tr '\\0' x; echo; echo FAILED: last words >&2; exit 3"}`))
	if err == nil {
		t.Fatal("exit 3 was not an error")
	}
	out := cutOutput("error: "+err.Error(), maxOutput)
	for _, want := range []string{"xxx", "bytes left out]", "FAILED: last words", "[exit code 3]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("result lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[output cut at") {
		t.Fatalf("the loop cut the result again:\n%s", out)
	}
}
