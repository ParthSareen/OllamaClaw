package telegram

import "testing"

func TestSplitText(t *testing.T) {
	text := "line1\nline2\nline3\nline4"
	chunks := splitText(text, 8)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks")
	}
	for _, c := range chunks {
		if len(c) > 8 {
			t.Fatalf("chunk too long: %d", len(c))
		}
	}
}
