package cfg

import (
	"encoding/json"
	"strings"
	"testing"
)

// The whole point of the string-aware scanner: a URL is not a comment.
func TestStripCommentsKeepsURLsAndPaths(t *testing.T) {
	src := []byte(`{
  "endpoint": "https://s3.example.com",   // the object store
  "composeFile": "C:\\server\\outline\\docker-compose.yml",
  /* block comment
     across lines */
  "quoted": "not a // comment and not a /* comment */ either",
  "escaped": "she said \"https://x\" loudly",
  "keepDaily": 7
}`)

	var got map[string]any
	if err := json.Unmarshal(stripComments(src), &got); err != nil {
		t.Fatalf("stripped output is not valid JSON: %v\n%s", err, stripComments(src))
	}

	want := map[string]string{
		"endpoint":    "https://s3.example.com",
		"composeFile": `C:\server\outline\docker-compose.yml`,
		"quoted":      "not a // comment and not a /* comment */ either",
		"escaped":     `she said "https://x" loudly`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %#v, want %#v", k, got[k], v)
		}
	}
	if got["keepDaily"] != float64(7) {
		t.Errorf("keepDaily = %#v", got["keepDaily"])
	}
}

// Offsets must not shift, or json's error positions point at the wrong line.
func TestStripCommentsPreservesLength(t *testing.T) {
	src := []byte("{\n  // hello\n  \"a\": 1 /* x */\n}")
	out := stripComments(src)
	if len(out) != len(src) {
		t.Fatalf("length changed: %d -> %d", len(src), len(out))
	}
	if strings.Count(string(out), "\n") != strings.Count(string(src), "\n") {
		t.Error("newline count changed; error line numbers would be wrong")
	}
}

func TestStripCommentsLeavesPlainJSONAlone(t *testing.T) {
	src := []byte(`{"a":1,"b":[2,3],"c":{"d":"e"}}`)
	if string(stripComments(src)) != string(src) {
		t.Errorf("plain JSON was altered:\n%s", stripComments(src))
	}
}
