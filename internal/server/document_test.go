package server

import (
	"context"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
	"github.com/mochizuki875/llm-file-gateway/internal/files"
)

type failingDocumentConverter struct {
	validateErr error
	convertErr  error
}

func (failingDocumentConverter) Extension() string { return ".internal" }

func (failingDocumentConverter) MediaType() string { return "application/octet-stream" }

func (documentConverter failingDocumentConverter) Validate(string) error {
	return documentConverter.validateErr
}

func (documentConverter failingDocumentConverter) Convert(context.Context, string, string, converter.Options) (converter.Result, error) {
	return converter.Result{}, documentConverter.convertErr
}

func TestPrepareDocumentHidesInternalConverterErrors(t *testing.T) {
	for _, test := range []struct {
		name          string
		validateErr   error
		conversionErr error
	}{
		{name: "validation", validateErr: errors.New("validation failed at /tmp/converter/source.internal")},
		{name: "conversion", conversionErr: errors.New("conversion failed at /tmp/converter/source.internal")},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings, dataStore, _ := testDependencies(t)
			documentConverter := failingDocumentConverter{validateErr: test.validateErr, convertErr: test.conversionErr}
			registry := converter.Registry{
				".internal": func(context.Context, converter.ConverterConfig, converter.ConverterHandle) (converter.DocumentConverter, error) {
					return documentConverter, nil
				},
			}
			service := files.New(settings, dataStore, converter.NewDispatcher(registry, converter.DefaultConverterConfig(), nil))
			server := &Server{settings: settings, files: service}
			temporary := make([]string, 0)
			t.Cleanup(func() {
				for _, directory := range temporary {
					_ = os.RemoveAll(directory)
				}
			})

			_, err := server.prepareDocument(context.Background(), map[string]any{
				"filename":  "notes.internal",
				"file_data": "aGVsbG8=",
			}, "tenant", "input[0].content[0]", &temporary)
			var gatewayError *apierror.Error
			if !errors.As(err, &gatewayError) {
				t.Fatalf("error = %v, want apierror.Error", err)
			}
			if gatewayError.Code != "file_processing_failed" || gatewayError.Message != `File processing failed for "notes.internal".` {
				t.Fatalf("error = %#v", gatewayError)
			}
			if strings.Contains(gatewayError.Message, "/tmp/") {
				t.Fatalf("error exposes internal path: %q", gatewayError.Message)
			}
		})
	}
}

func TestRequestDirectoryUsesOwnerOnlyPermissions(t *testing.T) {
	directory, err := createRequestDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("request directory mode = %04o, want 0700", got)
	}
}

func TestDocumentPartsReportsMissingArtifact(t *testing.T) {
	settings, _, _ := testDependencies(t)
	server := &Server{settings: settings}
	document := resolvedDocument{
		filename:   "notes.txt",
		derivedDir: t.TempDir(),
		manifest: converter.Manifest{Documents: []converter.ManifestDocument{{Parts: []converter.ManifestPart{{
			PartNumber: 1,
			TextPath:   "missing.txt",
		}}}}},
	}

	if _, err := server.documentParts(document, "responses", "input[0].content[0]"); err == nil {
		t.Fatal("documentParts() succeeded with a missing text artifact")
	}
}

func TestDocumentPartsEmitsArtifactTextBeforeImage(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.MaxDocumentPages = 1
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(derivedDir, "page.txt"), []byte("page text"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(derivedDir, "page.png"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	imagePath := "page.png"
	pageNumber := 1
	document := resolvedDocument{
		filename:   "document.docx",
		derivedDir: derivedDir,
		manifest: converter.Manifest{Documents: []converter.ManifestDocument{{Parts: []converter.ManifestPart{{
			PartNumber: 1, PageNumber: &pageNumber, TextPath: "page.txt", ImagePath: &imagePath,
		}}}}},
	}

	parts, err := server.documentParts(document, "responses", "input[0].content[0]")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("parts = %#v, want metadata, artifact text, and one image", parts)
	}
	metadata := parts[0].(map[string]any)
	if metadata["type"] != "input_text" || metadata["text"] != `<document_metadata filename="document.docx" />` {
		t.Fatalf("metadata part = %#v", metadata)
	}
	text := parts[1].(map[string]any)
	if text["type"] != "input_text" || !strings.Contains(text["text"].(string), "page text") || !strings.Contains(text["text"].(string), `page="1"`) {
		t.Fatalf("artifact text part = %#v", text)
	}
	if parts[2].(map[string]any)["type"] != "input_image" {
		t.Fatalf("image part = %#v", parts[2])
	}
}

func TestDocumentPartsEmitsMetadataBeforeImageWhenTextIsEmpty(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.MaxDocumentPages = 1
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(derivedDir, "page.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(derivedDir, "page.png"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	imagePath := "page.png"
	document := resolvedDocument{
		filename:   "document.pdf",
		derivedDir: derivedDir,
		manifest: converter.Manifest{Documents: []converter.ManifestDocument{{Parts: []converter.ManifestPart{{
			PartNumber: 1, TextPath: "page.txt", ImagePath: &imagePath,
		}}}}},
	}

	parts, err := server.documentParts(document, "responses", "input[0].content[0]")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %#v, want metadata and one input_image", parts)
	}
	metadata := parts[0].(map[string]any)
	if metadata["type"] != "input_text" || metadata["text"] != `<document_metadata filename="document.pdf" />` {
		t.Fatalf("metadata part = %#v", metadata)
	}
	if parts[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("image part = %#v", parts[1])
	}
}

func TestDocumentPartsUnlimitedPagesEmitsAllImages(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.MaxDocumentPages = 0
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	imagePath := "page.png"
	if err := os.WriteFile(filepath.Join(derivedDir, imagePath), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	document := resolvedDocument{
		filename:   "document.pdf",
		derivedDir: derivedDir,
		manifest: converter.Manifest{Documents: []converter.ManifestDocument{{Parts: []converter.ManifestPart{
			{PartNumber: 1, ImagePath: &imagePath},
			{PartNumber: 2, ImagePath: &imagePath},
			{PartNumber: 3, ImagePath: &imagePath},
		}}}},
	}

	parts, err := server.documentParts(document, "responses", "input[0].content[0]")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 4 {
		t.Fatalf("parts = %d, want metadata and 3 images (unlimited)", len(parts))
	}
	for _, part := range parts[1:] {
		if part.(map[string]any)["type"] != "input_image" {
			t.Fatalf("part = %#v, want input_image", part)
		}
	}
}

func TestDocumentPartsRejectsRenderedImageAboveTarget(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.DocumentImageTargetPixels = 3
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	imagePath := "page.png"
	writeTestPNG(t, filepath.Join(derivedDir, imagePath), 2, 2)
	width, height := 2, 2
	mediaType := "image/png"
	document := resolvedDocument{
		filename:   "document.pdf",
		derivedDir: derivedDir,
		manifest: converter.Manifest{
			Source: converter.ManifestSource{MediaType: "application/pdf"},
			Documents: []converter.ManifestDocument{{Parts: []converter.ManifestPart{{
				PartNumber: 1, ImagePath: &imagePath, Width: &width, Height: &height, MediaType: &mediaType,
			}}}},
		},
	}

	_, err := server.documentParts(document, "responses", "input[0].content[0]")
	var gatewayError *apierror.Error
	if !errors.As(err, &gatewayError) {
		t.Fatalf("error = %v, want apierror.Error", err)
	}
	if gatewayError.Status != 422 || gatewayError.Code != "image_too_large" || gatewayError.Param != "input[0].content[0]" {
		t.Fatalf("error = %#v", gatewayError)
	}
}

func TestDocumentPartsDoesNotLimitDirectImages(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.DocumentImageTargetPixels = 1
	settings.MaxDocumentPagePixels = 1
	settings.MaxDocumentPixels = 1
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	imagePath := "image.png"
	writeTestPNG(t, filepath.Join(derivedDir, imagePath), 2, 2)
	document := resolvedDocument{
		filename:   "image.png",
		derivedDir: derivedDir,
		manifest: converter.Manifest{
			Source:    converter.ManifestSource{MediaType: "image/png"},
			Documents: []converter.ManifestDocument{{Parts: []converter.ManifestPart{{PartNumber: 1, ImagePath: &imagePath}}}},
		},
	}

	parts, err := server.documentParts(document, "responses", "input[0].content[0]")
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[0].(map[string]any)["type"] != "input_text" || parts[1].(map[string]any)["type"] != "input_image" {
		t.Fatalf("parts = %#v, want metadata and one input_image", parts)
	}
}

func TestDocumentPartsRejectsRenderedImageManifestMismatch(t *testing.T) {
	settings, _, _ := testDependencies(t)
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	imagePath := "page.png"
	writeTestPNG(t, filepath.Join(derivedDir, imagePath), 2, 2)
	wrongWidth := 3
	document := resolvedDocument{
		filename:   "document.pdf",
		derivedDir: derivedDir,
		manifest: converter.Manifest{
			Source:    converter.ManifestSource{MediaType: "application/pdf"},
			Documents: []converter.ManifestDocument{{Parts: []converter.ManifestPart{{PartNumber: 1, ImagePath: &imagePath, Width: &wrongWidth}}}},
		},
	}

	if _, err := server.documentParts(document, "responses", "input[0].content[0]"); err == nil || !strings.Contains(err.Error(), "width does not match") {
		t.Fatalf("error = %v, want manifest width mismatch", err)
	}
}

func TestDocumentPartsEnforcesRenderedDocumentPixelLimit(t *testing.T) {
	settings, _, _ := testDependencies(t)
	settings.DocumentImageTargetPixels = 0
	settings.MaxDocumentPixels = 7
	server := &Server{settings: settings}
	derivedDir := t.TempDir()
	imagePath := "page.png"
	writeTestPNG(t, filepath.Join(derivedDir, imagePath), 2, 2)
	document := resolvedDocument{
		filename:   "document.pdf",
		derivedDir: derivedDir,
		manifest: converter.Manifest{
			Source: converter.ManifestSource{MediaType: "application/pdf"},
			Documents: []converter.ManifestDocument{{Parts: []converter.ManifestPart{
				{PartNumber: 1, ImagePath: &imagePath},
				{PartNumber: 2, ImagePath: &imagePath},
			}}},
		},
	}

	_, err := server.documentParts(document, "responses", "input[0].content[0]")
	var gatewayError *apierror.Error
	if !errors.As(err, &gatewayError) || gatewayError.Code != "image_too_large" {
		t.Fatalf("error = %v, want image_too_large", err)
	}
}

func writeTestPNG(t *testing.T, path string, width, height int) {
	t.Helper()
	output, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(output, image.NewRGBA(image.Rect(0, 0, width, height))); err != nil {
		_ = output.Close()
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDocumentPartsRejectsMissingDocuments(t *testing.T) {
	server := &Server{}
	if _, err := server.documentParts(resolvedDocument{}, "responses", "input[0].content[0]"); err == nil {
		t.Fatal("manifest without documents accepted")
	}
}
