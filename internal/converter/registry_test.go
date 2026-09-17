package converter

import (
	"context"
	"strings"
	"testing"
)

type testConverter struct {
	extension      string
	mediaType      string
	validateCalled bool
	convertCalled  bool
	options        Options
}

func (documentConverter *testConverter) Extension() string { return documentConverter.extension }
func (documentConverter *testConverter) MediaType() string { return documentConverter.mediaType }
func (documentConverter *testConverter) Validate(string) error {
	documentConverter.validateCalled = true
	return nil
}
func (documentConverter *testConverter) Convert(_ context.Context, _, _ string, options Options) (Result, error) {
	documentConverter.convertCalled = true
	documentConverter.options = options
	return Result{Warnings: []string{"custom"}}, nil
}

func TestRegistryAcceptsConverterPlugins(t *testing.T) {
	plugin := &testConverter{extension: ".custom", mediaType: "application/x-custom"}
	registry, err := NewRegistry(plugin)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := registry.Converter(".CUSTOM")
	if err != nil {
		t.Fatal(err)
	}
	if registered.MediaType() != plugin.mediaType {
		t.Fatalf("media type = %q, want %q", registered.MediaType(), plugin.mediaType)
	}
	if _, err := NewRegistry(plugin, plugin); err == nil {
		t.Fatal("duplicate extension registration succeeded")
	}
}

func TestDefaultRegistryContainsSupportedFormats(t *testing.T) {
	want := []string{".csv", ".doc", ".docx", ".htm", ".html", ".jpeg", ".jpg", ".markdown", ".md", ".pdf", ".png", ".ppt", ".pptx", ".txt", ".xls", ".xlsm", ".xlsx"}
	got := defaultRegistry.Extensions()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("extensions = %v, want %v", got, want)
	}
	for extension, plugin := range defaultRegistry.byExtension {
		if plugin.Extension() != extension {
			t.Fatalf("plugin extension = %q, registry key = %q", plugin.Extension(), extension)
		}
	}
}
