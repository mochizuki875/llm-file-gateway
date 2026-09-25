package converter

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatcherDelegatesToSelectedPlugin(t *testing.T) {
	plugin := &testConverter{extension: ".custom", mediaType: "application/x-custom"}
	registry := Registry{}
	if err := registry.Register(".custom", ConverterAdapter(func(ConverterConfig) DocumentConverter { return plugin })); err != nil {
		t.Fatal(err)
	}
	options := Options{MaxPages: 7, MaxTextChars: 1234}
	documentConverter, err := NewDispatcher(registry, DefaultConverterConfig(), nil).ResolveConverter(context.Background(), "document.CUSTOM")
	if err != nil {
		t.Fatal(err)
	}
	if documentConverter != plugin || plugin.validateCalled || plugin.convertCalled {
		t.Fatalf("resolved converter = %#v, calls = validate:%t convert:%t", documentConverter, plugin.validateCalled, plugin.convertCalled)
	}
	if err := documentConverter.Validate("document.CUSTOM"); err != nil {
		t.Fatal(err)
	}
	result, err := documentConverter.Convert(context.Background(), "document.CUSTOM", t.TempDir(), options)
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

// TestDispatcherFallsBackOnlyForUnknownExtensions verifies that the plain-text
// fallback is used only when no converter is registered for the extension.
// A factory failure must be propagated instead of being silently replaced by
// the fallback converter.
func TestDispatcherFallsBackOnlyForUnknownExtensions(t *testing.T) {
	registry := Registry{}
	if err := registry.Register(".custom", ConverterAdapter(func(ConverterConfig) DocumentConverter {
		return &testConverter{extension: ".custom", mediaType: "application/x-custom"}
	})); err != nil {
		t.Fatal(err)
	}
	dispatcher := NewDispatcher(registry, DefaultConverterConfig(), nil)

	// Unknown extension: fallback to plain text.
	documentConverter, err := dispatcher.ResolveConverter(context.Background(), "document.unknown")
	if err != nil {
		t.Fatalf("unknown extension: %v", err)
	}
	if documentConverter.MediaType() != "text/plain" {
		t.Fatalf("unknown extension media type = %q, want text/plain", documentConverter.MediaType())
	}

	// Registered extension: the registered converter is returned.
	documentConverter, err = dispatcher.ResolveConverter(context.Background(), "document.custom")
	if err != nil {
		t.Fatalf("registered extension: %v", err)
	}
	if documentConverter.MediaType() != "application/x-custom" {
		t.Fatalf("registered extension media type = %q, want application/x-custom", documentConverter.MediaType())
	}

	// Factory failure: the error is propagated, not swallowed by the fallback.
	failing := Registry{}
	if err := failing.Register(".broken", func(context.Context, ConverterConfig, ConverterHandle) (DocumentConverter, error) {
		return nil, errors.New("factory exploded")
	}); err != nil {
		t.Fatal(err)
	}
	brokenDispatcher := NewDispatcher(failing, DefaultConverterConfig(), nil)
	if _, err := brokenDispatcher.ResolveConverter(context.Background(), "document.broken"); err == nil || !strings.Contains(err.Error(), "factory exploded") {
		t.Fatalf("factory failure = %v, want propagated error", err)
	}
}

func TestImagePluginsPreserveOriginalFormat(t *testing.T) {
	for _, name := range []string{"samplefile.jpg", "samplefile.jpeg", "samplefile.png"} {
		t.Run(name, func(t *testing.T) {
			source := filepath.Join("..", "..", "example", name)
			output := filepath.Join(t.TempDir(), "derived")
			result, err := convertForTest(context.Background(), source, output, Options{MaxPages: 20, MaxTextChars: 500_000})
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

func convertForTest(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	documentConverter, err := newDefaultDispatcherForTest().ResolveConverter(ctx, source)
	if err != nil {
		return Result{}, err
	}
	if err := documentConverter.Validate(source); err != nil {
		return Result{}, err
	}
	return documentConverter.Convert(ctx, source, outputDir, options)
}

func newDefaultDispatcherForTest() *Dispatcher {
	return NewDispatcher(NewInTreeRegistry(), DefaultConverterConfig(), nil)
}
