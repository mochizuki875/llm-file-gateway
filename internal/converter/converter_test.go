package converter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"
)

func TestLimitErrors(t *testing.T) {
	pageErr := &PageLimitError{Limit: 20}
	if pageErr.Error() != "document exceeds the 20-page limit" {
		t.Fatalf("PageLimitError.Error() = %q", pageErr.Error())
	}
	textErr := &TextLimitError{Limit: 500}
	if textErr.Error() != "document exceeds the 500-character text limit" {
		t.Fatalf("TextLimitError.Error() = %q", textErr.Error())
	}
}

func TestIsRetryable(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: true},
		{name: "generic", err: errors.New("boom"), want: true},
		{name: "wrapped_generic", err: fmt.Errorf("document conversion: %w", errors.New("boom")), want: true},
		{name: "page_limit", err: &PageLimitError{Limit: 20}, want: false},
		{name: "wrapped_page_limit", err: fmt.Errorf("document conversion: %w", &PageLimitError{Limit: 20}), want: false},
		{name: "text_limit", err: &TextLimitError{Limit: 500}, want: false},
		{name: "wrapped_text_limit", err: fmt.Errorf("document conversion: %w", &TextLimitError{Limit: 500}), want: false},
		{name: "validation", err: &extractor.ValidationError{Message: "text file must be valid UTF-8"}, want: false},
		{name: "wrapped_validation", err: fmt.Errorf("document conversion: %w", &extractor.ValidationError{Message: "text file must be valid UTF-8"}), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := IsRetryable(test.err); got != test.want {
				t.Fatalf("IsRetryable(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestValidateSignature(t *testing.T) {
	root := t.TempDir()
	valid := filepath.Join(root, "valid.pdf")
	if err := os.WriteFile(valid, []byte("%PDF-1.7\ncontent"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateSignature(valid, []byte("%PDF-")); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	mismatch := filepath.Join(root, "mismatch.pdf")
	if err := os.WriteFile(mismatch, []byte("not a pdf"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateSignature(mismatch, []byte("%PDF-")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatch error = %v", err)
	}

	short := filepath.Join(root, "short.pdf")
	if err := os.WriteFile(short, []byte("%P"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateSignature(short, []byte("%PDF-")); err == nil {
		t.Fatal("short file accepted")
	}

	missing := filepath.Join(root, "missing.pdf")
	if err := validateSignature(missing, []byte("%PDF-")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestConverterMetadata(t *testing.T) {
	registry := NewInTreeRegistry()
	for _, test := range []struct {
		extension string
		mediaType string
	}{
		{".pdf", "application/pdf"},
		{".doc", "application/msword"},
		{".docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
		{".ppt", "application/vnd.ms-powerpoint"},
		{".pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"},
		{".xls", "application/vnd.ms-excel"},
		{".xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"},
		{".xlsm", "application/vnd.ms-excel.sheet.macroEnabled.12"},
		{".jpg", "image/jpeg"},
		{".jpeg", "image/jpeg"},
		{".png", "image/png"},
		{".txt", "text/plain"},
		{".md", "text/markdown"},
		{".markdown", "text/markdown"},
		{".csv", "text/csv"},
		{".html", "text/html"},
		{".htm", "text/html"},
	} {
		t.Run(test.extension, func(t *testing.T) {
			documentConverter, err := registry.Converter(context.Background(), test.extension, DefaultConverterConfig(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if documentConverter.Extension() != test.extension {
				t.Fatalf("Extension() = %q, want %q", documentConverter.Extension(), test.extension)
			}
			if documentConverter.MediaType() != test.mediaType {
				t.Fatalf("MediaType() = %q, want %q", documentConverter.MediaType(), test.mediaType)
			}
		})
	}
}

func TestSignatureValidationPerConverter(t *testing.T) {
	root := t.TempDir()
	write := func(name string, content []byte) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	zipFile := write("archive.docx", []byte("PK\x03\x04rest"))
	oleFile := write("legacy.doc", []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1, 0x00})
	pdfFile := write("document.pdf", []byte("%PDF-1.4"))

	for _, test := range []struct {
		name      string
		converter DocumentConverter
		validPath string
		invalid   string
	}{
		{name: "pdf", converter: pdfConverter{}, validPath: pdfFile, invalid: "not a pdf"},
		{name: "doc", converter: docConverter{}, validPath: oleFile, invalid: "not ole"},
		{name: "docx", converter: docxConverter{}, validPath: zipFile, invalid: "not zip"},
		{name: "ppt", converter: pptConverter{}, validPath: oleFile, invalid: "not ole"},
		{name: "pptx", converter: pptxConverter{}, validPath: zipFile, invalid: "not zip"},
		{name: "xls", converter: xlsConverter{}, validPath: oleFile, invalid: "not ole"},
		{name: "xlsx", converter: xlsxConverter{}, validPath: zipFile, invalid: "not zip"},
		{name: "xlsm", converter: xlsmConverter{}, validPath: zipFile, invalid: "not zip"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.converter.Validate(test.validPath); err != nil {
				t.Fatalf("valid file rejected: %v", err)
			}
			invalidPath := write(test.name+".invalid", []byte(test.invalid))
			if err := test.converter.Validate(invalidPath); err == nil {
				t.Fatal("invalid file accepted")
			}
		})
	}
}

func TestImageValidation(t *testing.T) {
	root := t.TempDir()
	pngPath := filepath.Join(root, "image.png")
	if err := writePNG(pngPath, 4, 3); err != nil {
		t.Fatal(err)
	}
	if err := (pngConverter{}).Validate(pngPath); err != nil {
		t.Fatalf("valid PNG rejected: %v", err)
	}
	if err := (jpgConverter{}).Validate(pngPath); err == nil {
		t.Fatal("PNG accepted as JPEG")
	}
	invalid := filepath.Join(root, "broken.png")
	if err := os.WriteFile(invalid, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (pngConverter{}).Validate(invalid); err == nil {
		t.Fatal("broken image accepted")
	}
}

func TestConvertImageDocument(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "photo.png")
	if err := writePNG(source, 8, 6); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "derived")
	result, err := convertImageDocument(source, output, "image/png", 8, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(result.Artifacts))
	}
	artifact := result.Artifacts[0]
	if artifact.ImagePath == nil || artifact.Width == nil || artifact.Height == nil || artifact.MediaType == nil || artifact.SHA256 == nil {
		t.Fatalf("incomplete artifact: %#v", artifact)
	}
	if *artifact.Width != 8 || *artifact.Height != 6 || *artifact.MediaType != "image/png" {
		t.Fatalf("artifact metadata = %#v", artifact)
	}
	converted, err := os.ReadFile(filepath.Join(output, *artifact.ImagePath))
	if err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(source)
	if !bytes.Equal(converted, original) {
		t.Fatal("image bytes changed")
	}
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	if result.Manifest.SchemaVersion != 3 || result.Manifest.Source.MediaType != "image/png" {
		t.Fatalf("manifest = %#v", result.Manifest)
	}
}

func TestConvertImageDocumentRejectsMissingSource(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "derived")
	if _, err := convertImageDocument(filepath.Join(root, "missing.png"), output, "image/png", 1, 1); err == nil {
		t.Fatal("missing source succeeded")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("failed conversion output still exists: %v", err)
	}
}

func TestConvertTextDocumentWritesParts(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(source, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "derived")
	result, err := convertTextDocument(source, output, "text/plain", []string{"part one", "part two"}, Options{MaxTextChars: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2", len(result.Artifacts))
	}
	for index, artifact := range result.Artifacts {
		content, err := os.ReadFile(filepath.Join(output, artifact.TextPath))
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"part one", "part two"}[index]
		if string(content) != want {
			t.Fatalf("part %d = %q, want %q", index, content, want)
		}
	}
}

func TestConvertTextDocumentEnforcesTextLimit(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "notes.txt")
	if err := os.WriteFile(source, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := convertTextDocument(source, filepath.Join(root, "derived"), "text/plain", []string{"12345"}, Options{MaxTextChars: 4})
	var limitError *TextLimitError
	if !errors.As(err, &limitError) || limitError.Limit != 4 {
		t.Fatalf("error = %v, want TextLimitError", err)
	}
}

func writePNG(path string, width, height int) error {
	output, err := os.Create(path)
	if err != nil {
		return err
	}
	defer output.Close()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 40), B: 128, A: 255})
		}
	}
	return png.Encode(output, img)
}
