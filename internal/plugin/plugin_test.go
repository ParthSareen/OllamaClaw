package plugin

import "testing"

func TestValidateManifest(t *testing.T) {
	m := Manifest{
		ID:      "example",
		Name:    "Example",
		Version: "1.0.0",
		Entrypoint: Entrypoint{
			Command: "python3",
			Args:    []string{"plugin.py"},
		},
		Protocol: Protocol{Transport: "stdio", Framing: "ndjson"},
	}
	if err := validateManifest(m); err != nil {
		t.Fatalf("validateManifest() error: %v", err)
	}
}

func TestParseGitSource(t *testing.T) {
	src, ref := parseGitSource("git:https://github.com/acme/repo.git@v1.2.3")
	if src != "https://github.com/acme/repo.git" || ref != "v1.2.3" {
		t.Fatalf("unexpected parse: src=%s ref=%s", src, ref)
	}
}

func TestIsArchiveURL(t *testing.T) {
	if !isArchiveURL("https://example.com/plugin.zip") {
		t.Fatalf("expected zip archive URL")
	}
	if isArchiveURL("https://example.com/repo.git") {
		t.Fatalf("did not expect git URL to be archive")
	}
}
