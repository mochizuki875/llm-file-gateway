package converter

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
