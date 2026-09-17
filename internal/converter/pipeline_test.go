package converter

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateTextLimit(t *testing.T) {
	err := validateTextLimit([]string{"12345"}, 4)
	var limitError *TextLimitError
	if !errors.As(err, &limitError) || limitError.Limit != 4 {
		t.Fatalf("validateTextLimit() error = %v, want 4-character TextLimitError", err)
	}
}

func TestConversionReplacesOutputAndCleansUpFailure(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes.txt")
	output := filepath.Join(root, "derived")
	if err := os.WriteFile(source, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := convertTextDocument(source, output, "text/plain", []string{"current"}, Options{MaxTextChars: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale artifact still exists: %v", err)
	}

	missingSource := filepath.Join(root, "missing.txt")
	if _, err := convertTextDocument(missingSource, output, "text/plain", []string{"partial"}, Options{MaxTextChars: 100}); err == nil {
		t.Fatal("conversion with missing source succeeded")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed conversion output still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("failed conversion manifest still exists: %v", err)
	}
}
