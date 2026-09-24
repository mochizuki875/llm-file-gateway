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
	registry := Registry{}
	if err := registry.Register(".custom", ConverterAdapter(func(ConverterConfig) DocumentConverter { return plugin })); err != nil {
		t.Fatal(err)
	}
	registered, err := registry.Converter(context.Background(), ".CUSTOM", DefaultConverterConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if registered.MediaType() != plugin.mediaType {
		t.Fatalf("media type = %q, want %q", registered.MediaType(), plugin.mediaType)
	}
	if err := registry.Register(".custom", ConverterAdapter(func(ConverterConfig) DocumentConverter { return plugin })); err == nil {
		t.Fatal("duplicate extension registration succeeded")
	}
}

func TestRegistryRejectsInvalidRegistrations(t *testing.T) {
	registry := Registry{}
	if err := registry.Register("custom", ConverterAdapter(func(ConverterConfig) DocumentConverter { return &testConverter{} })); err == nil {
		t.Fatal("extension without leading dot accepted")
	}
	if err := registry.Register(".custom", nil); err == nil {
		t.Fatal("nil factory accepted")
	}
	if err := registry.Unregister(".missing"); err == nil {
		t.Fatal("unregister of missing extension succeeded")
	}
}

func TestRegistryMerge(t *testing.T) {
	left := Registry{}
	right := Registry{}
	if err := left.Register(".custom", ConverterAdapter(func(ConverterConfig) DocumentConverter { return &testConverter{} })); err != nil {
		t.Fatal(err)
	}
	if err := right.Register(".other", ConverterAdapter(func(ConverterConfig) DocumentConverter { return &testConverter{} })); err != nil {
		t.Fatal(err)
	}
	if err := left.Merge(right); err != nil {
		t.Fatal(err)
	}
	if _, err := left.Converter(context.Background(), ".other", DefaultConverterConfig(), nil); err != nil {
		t.Fatalf("merged converter unavailable: %v", err)
	}
	if err := left.Merge(right); err == nil {
		t.Fatal("merge with conflicting extension succeeded")
	}
}

func TestInTreeRegistryContainsSupportedFormats(t *testing.T) {
	want := []string{".csv", ".doc", ".docx", ".htm", ".html", ".jpeg", ".jpg", ".markdown", ".md", ".pdf", ".png", ".ppt", ".pptx", ".txt", ".xls", ".xlsm", ".xlsx"}
	registry := NewInTreeRegistry()
	got := registry.Extensions()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("extensions = %v, want %v", got, want)
	}
	for extension, factory := range registry {
		documentConverter, err := factory(context.Background(), DefaultConverterConfig(), nil)
		if err != nil {
			t.Fatalf("factory for %s failed: %v", extension, err)
		}
		if documentConverter.Extension() != extension {
			t.Fatalf("plugin extension = %q, registry key = %q", documentConverter.Extension(), extension)
		}
	}
}
