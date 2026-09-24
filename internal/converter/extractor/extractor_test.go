package extractor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSource(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlainTextReturnsContentAsIs(t *testing.T) {
	source := writeSource(t, "notes.txt", []byte("hello\nworld\n"))
	text, err := PlainText(source)
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello\nworld\n" {
		t.Fatalf("text = %q", text)
	}
}

func TestReadUTF8StripsBOM(t *testing.T) {
	source := writeSource(t, "bom.txt", []byte("\xef\xbb\xbfcontent"))
	text, err := PlainText(source)
	if err != nil {
		t.Fatal(err)
	}
	if text != "content" {
		t.Fatalf("text = %q, want %q", text, "content")
	}
}

func TestReadUTF8RejectsInvalidEncoding(t *testing.T) {
	source := writeSource(t, "invalid.txt", []byte{0xff, 0xfe, 0x00})
	if _, err := PlainText(source); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("error = %v, want UTF-8 error", err)
	}
}

func TestReadUTF8RejectsNullBytes(t *testing.T) {
	source := writeSource(t, "binary.txt", []byte{'a', 0, 'b'})
	if _, err := PlainText(source); err == nil || !strings.Contains(err.Error(), "null bytes") {
		t.Fatalf("error = %v, want null byte error", err)
	}
}

func TestReadUTF8MissingFile(t *testing.T) {
	if _, err := PlainText(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Fatal("missing file succeeded")
	}
}

func TestCSVJoinsRecordsWithTabs(t *testing.T) {
	source := writeSource(t, "records.csv", []byte("name,value\nanswer,\"4,821\"\n"))
	text, err := CSV(source)
	if err != nil {
		t.Fatal(err)
	}
	if text != "name\tvalue\nanswer\t4,821" {
		t.Fatalf("text = %q", text)
	}
}

func TestCSVRejectsMalformedInput(t *testing.T) {
	source := writeSource(t, "broken.csv", []byte("a,b\n\"unterminated"))
	if _, err := CSV(source); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("error = %v, want malformed CSV error", err)
	}
}

func TestCSVRejectsInvalidUTF8(t *testing.T) {
	source := writeSource(t, "binary.csv", []byte{0xff, 0xfe})
	if _, err := CSV(source); err == nil {
		t.Fatal("invalid UTF-8 CSV succeeded")
	}
}

func TestHTMLExtractsVisibleText(t *testing.T) {
	source := writeSource(t, "page.html", []byte(
		"<html><head><title>hidden</title></head><body><h1>Report</h1><p>Hello <b>world</b>.</p><script>bad()</script><style>.x{}</style><template>t</template></body></html>",
	))
	text, err := HTML(source)
	if err != nil {
		t.Fatal(err)
	}
	if text != "Report\nHello world." {
		t.Fatalf("text = %q", text)
	}
}

func TestHTMLNormalizesWhitespace(t *testing.T) {
	source := writeSource(t, "spaces.html", []byte("<p>  a   b  </p><p>c</p>"))
	text, err := HTML(source)
	if err != nil {
		t.Fatal(err)
	}
	if text != "a b\nc" {
		t.Fatalf("text = %q", text)
	}
}

func TestHTMLRejectsInvalidUTF8(t *testing.T) {
	source := writeSource(t, "binary.html", []byte{0xff, 0xfe})
	if _, err := HTML(source); err == nil {
		t.Fatal("invalid UTF-8 HTML succeeded")
	}
}
