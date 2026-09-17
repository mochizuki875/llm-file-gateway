package converter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTextConverters(t *testing.T) {
	tests := map[string]struct {
		content string
		want    string
	}{
		"notes.md":    {"# Heading\n\nBody\n", "# Heading\n\nBody\n"},
		"records.csv": {"name,value\nanswer,\"4,821\"\n", "name\tvalue\nanswer\t4,821"},
		"page.html":   {"<head><title>hidden</title></head><h1>Report</h1><p>Hello <b>world</b>.</p><script>bad()</script>", "Report\nHello world."},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, name)
			if err := os.WriteFile(source, []byte(test.content), 0o644); err != nil {
				t.Fatal(err)
			}
			result, err := Convert(context.Background(), source, filepath.Join(root, "derived"), Options{MaxPages: 20, MaxTextChars: 500_000})
			if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(root, "derived", result.Artifacts[0].TextPath))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != test.want {
				t.Fatalf("text = %q, want %q", content, test.want)
			}
			if result.Manifest.SchemaVersion != 2 || result.Artifacts[0].ImagePath != nil {
				t.Fatalf("unexpected result: %#v", result)
			}
		})
	}
}

func TestRejectsInvalidTextAndUnsupportedFiles(t *testing.T) {
	root := t.TempDir()
	invalid := filepath.Join(root, "invalid.txt")
	if err := os.WriteFile(invalid, []byte{0xff, 0xfe}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(invalid); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	if Supported(".rtf") {
		t.Fatal(".rtf unexpectedly supported")
	}
}

func TestUnknownTextFormatsUsePlainTextFallback(t *testing.T) {
	for _, name := range []string{"data.json", "events.jsonl", "config.yaml", "main.go"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, name)
			want := "plain text content\n"
			if err := os.WriteFile(source, []byte(want), 0o644); err != nil {
				t.Fatal(err)
			}
			if mediaType, err := MediaType(filepath.Ext(name)); err != nil || mediaType != "text/plain" {
				t.Fatalf("MediaType() = %q, %v; want text/plain", mediaType, err)
			}
			result, err := Convert(context.Background(), source, filepath.Join(root, "derived"), Options{MaxTextChars: 500_000})
			if err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(root, "derived", result.Artifacts[0].TextPath))
			if err != nil {
				t.Fatal(err)
			}
			if string(content) != want {
				t.Fatalf("text = %q, want %q", content, want)
			}
		})
	}

	root := t.TempDir()
	binary := filepath.Join(root, "binary.unknown")
	if err := os.WriteFile(binary, []byte{'P', 'K', 0, 1, 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Validate(binary); err == nil || !strings.Contains(err.Error(), "null bytes") {
		t.Fatalf("binary validation error = %v", err)
	}
}
