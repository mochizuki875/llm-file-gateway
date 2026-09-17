package converter

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatcherDelegatesToSelectedPlugin(t *testing.T) {
	plugin := &testConverter{extension: ".custom", mediaType: "application/x-custom"}
	registry, err := NewRegistry(plugin)
	if err != nil {
		t.Fatal(err)
	}
	options := Options{MaxPages: 7, MaxTextChars: 1234}
	result, err := NewDispatcher(registry).Convert(context.Background(), "document.CUSTOM", t.TempDir(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !plugin.validateCalled || !plugin.convertCalled || plugin.options != options {
		t.Fatalf("plugin calls = validate:%t convert:%t options:%+v", plugin.validateCalled, plugin.convertCalled, plugin.options)
	}
	if len(result.Warnings) != 1 || result.Warnings[0] != "custom" {
		t.Fatalf("result = %#v", result)
	}
}

func TestImagePluginsPreserveOriginalFormat(t *testing.T) {
	for _, name := range []string{"samplefile.jpg", "samplefile.jpeg", "samplefile.png"} {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join("..", "..", "example", name)
			output := filepath.Join(t.TempDir(), "derived")
			result, err := Convert(context.Background(), source, output, Options{MaxPages: 20, MaxTextChars: 500_000})
			if err != nil {
				t.Fatal(err)
			}
			artifact := result.Artifacts[0]
			if artifact.ImagePath == nil || artifact.Width == nil || artifact.Height == nil {
				t.Fatalf("incomplete image artifact: %#v", artifact)
			}
			original, _ := os.ReadFile(source)
			converted, _ := os.ReadFile(filepath.Join(output, *artifact.ImagePath))
			if !bytes.Equal(original, converted) {
				t.Fatal("image plugin changed source bytes")
			}
		})
	}
}

func TestPDFSampleExtractionAndRendering(t *testing.T) {
	if testing.Short() {
		t.Skip("PDFium rendering is an integration test")
	}
	source := filepath.Join("..", "..", "example", "samplefile.pdf")
	for _, test := range []struct {
		name        string
		disableText bool
		wantText    bool
	}{
		{name: "text_enabled", wantText: true},
		{name: "text_disabled", disableText: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "derived")
			result, err := Convert(context.Background(), source, output, Options{
				MaxPages: 20, MaxTextChars: 500_000, DisableTextExtraction: test.disableText,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Artifacts) != 2 {
				t.Fatalf("artifacts = %d, want 2", len(result.Artifacts))
			}
			if result.Artifacts[0].ImagePath == nil {
				t.Fatal("PDF artifact has no image")
			}
			text, err := os.ReadFile(filepath.Join(output, result.Artifacts[0].TextPath))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(string(text)) != ""; got != test.wantText {
				t.Fatalf("has extracted text = %t, want %t", got, test.wantText)
			}
		})
	}
}

func TestOfficePluginSampleConversion(t *testing.T) {
	if testing.Short() {
		t.Skip("LibreOffice rendering is an integration test")
	}
	for _, name := range []string{
		"samplefile.doc", "samplefile.docx", "samplefile.ppt", "samplefile.pptx",
		"samplefile.xls", "samplefile.xlsx", "samplefile.xlsm",
	} {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join("..", "..", "example", name)
			output := filepath.Join(t.TempDir(), "derived")
			result, err := Convert(context.Background(), source, output, Options{MaxPages: 99, MaxTextChars: 500_000})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Artifacts) == 0 || result.Artifacts[0].ImagePath == nil {
				t.Fatalf("Office plugin produced no page image: %#v", result.Artifacts)
			}
			var extracted strings.Builder
			for _, artifact := range result.Artifacts {
				content, err := os.ReadFile(filepath.Join(output, artifact.TextPath))
				if err != nil {
					t.Fatal(err)
				}
				extracted.Write(content)
			}
			if strings.TrimSpace(extracted.String()) == "" {
				t.Fatal("Office plugin produced no extracted text")
			}
		})
	}
}
