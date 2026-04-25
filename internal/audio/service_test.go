package audio

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ParthSareen/OllamaClaw/internal/config"
)

func TestPrepareSpeechTextStripsMarkdownAndCode(t *testing.T) {
	in := "Done.\n\n```go\nfmt.Println(\"hi\")\n```\n\nSee [`file.go`](https://example.com) for **details**."
	got := PrepareSpeechText(in, 1000)
	if strings.Contains(got, "```") || strings.Contains(got, "**") || strings.Contains(got, "https://") {
		t.Fatalf("expected markdown to be stripped, got %q", got)
	}
	if !strings.Contains(got, "I included a code block in the text reply.") {
		t.Fatalf("expected code block spoken placeholder, got %q", got)
	}
	if !strings.Contains(got, "file.go") {
		t.Fatalf("expected link label to remain, got %q", got)
	}
}

func TestPrepareSpeechTextTruncatesAtSentence(t *testing.T) {
	in := "First sentence. Second sentence has more detail. Third sentence should be omitted."
	got := PrepareSpeechText(in, 42)
	if !strings.HasPrefix(got, "First sentence.") {
		t.Fatalf("expected sentence prefix, got %q", got)
	}
	if !strings.Contains(got, "Full details are in the text reply.") {
		t.Fatalf("expected details suffix, got %q", got)
	}
}

func TestCleanTranscriptOutputRemovesOllamaCLIChatter(t *testing.T) {
	in := "\x1b[?25lAdded audio '/tmp/a.wav'\n\x1b[1GThinking...\nHello world.\n...done thinking.\n"
	got := cleanTranscriptOutput(in)
	if got != "Hello world." {
		t.Fatalf("cleanTranscriptOutput() = %q", got)
	}
}

func TestParseKokoroDuration(t *testing.T) {
	got, err := parseKokoroDuration([]byte("warning\n{\"duration_seconds\": 1.25}\n"))
	if err != nil {
		t.Fatalf("parseKokoroDuration() error: %v", err)
	}
	if got != 1.25 {
		t.Fatalf("expected 1.25, got %v", got)
	}
}

func TestSynthesizeLiveWhenEnabled(t *testing.T) {
	if os.Getenv("OLLAMACLAW_LIVE_KOKORO_TEST") != "1" {
		t.Skip("set OLLAMACLAW_LIVE_KOKORO_TEST=1 to run Kokoro synthesis")
	}
	svc := NewServiceFromConfig(config.Default())
	out, err := svc.Synthesize(context.Background(), "Kokoro live test from the OllamaClaw Go audio service.")
	if err != nil {
		t.Fatalf("Synthesize() error: %v", err)
	}
	defer out.Cleanup()
	if out.DurationSeconds <= 0 {
		t.Fatalf("expected positive duration, got %d", out.DurationSeconds)
	}
	if _, err := os.Stat(out.Path); err != nil {
		t.Fatalf("expected generated voice file: %v", err)
	}
	if _, err := os.Stat(out.WAVPath); err != nil {
		t.Fatalf("expected generated wav file: %v", err)
	}
}

func TestTranscribeLiveWhenEnabled(t *testing.T) {
	if os.Getenv("OLLAMACLAW_LIVE_OLLAMA_AUDIO_TEST") != "1" {
		t.Skip("set OLLAMACLAW_LIVE_OLLAMA_AUDIO_TEST=1 to run local Gemma audio transcription")
	}
	dir := t.TempDir()
	aiffPath := filepath.Join(dir, "input.aiff")
	if out, err := exec.Command("say", "-o", aiffPath, "OllamaClaw voice reply test.").CombinedOutput(); err != nil {
		t.Fatalf("say failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	svc := NewServiceFromConfig(config.Default())
	got, err := svc.Transcribe(context.Background(), aiffPath)
	if err != nil {
		t.Fatalf("Transcribe() error: %v", err)
	}
	gotLower := strings.ToLower(got)
	if !strings.Contains(gotLower, "voice") && !strings.Contains(gotLower, "test") {
		t.Fatalf("transcript did not contain expected words: %q", got)
	}
}
