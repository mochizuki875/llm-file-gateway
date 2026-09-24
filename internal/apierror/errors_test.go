package apierror

import (
	"strings"
	"testing"
)

func TestErrorImplementsError(t *testing.T) {
	err := New(400, "invalid_request", "Bad request.", "file")
	if err.Error() != "Bad request." {
		t.Fatalf("Error() = %q, want %q", err.Error(), "Bad request.")
	}
	if err.Status != 400 || err.Code != "invalid_request" || err.Param != "file" {
		t.Fatalf("error fields = %+v", err)
	}
}

func TestFileTooLargeFormatsLimit(t *testing.T) {
	for _, test := range []struct {
		name     string
		maxBytes int64
		want     string
	}{
		{name: "bytes", maxBytes: 500, want: "500 bytes"},
		{name: "kib", maxBytes: 2048, want: "2048 bytes (2 KiB)"},
		{name: "mib", maxBytes: 50 * 1024 * 1024, want: "52428800 bytes (50 MiB)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := FileTooLarge(test.maxBytes, "file")
			if err.Status != 400 || err.Code != "file_too_large" || err.Param != "file" {
				t.Fatalf("error fields = %+v", err)
			}
			if !strings.Contains(err.Message, test.want) {
				t.Fatalf("message = %q, want to contain %q", err.Message, test.want)
			}
		})
	}
}
