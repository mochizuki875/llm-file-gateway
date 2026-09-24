package converter

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mochizuki875/document-image-renderer/pkg/renderer"
	"github.com/mochizuki875/llm-file-gateway/internal/config"
	"github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"
)

// ConverterConfig holds the static configuration passed to converter factories
// when they are instantiated.
type ConverterConfig struct {
	DPI                int
	ImageFormat        renderer.ImageFormat
	LibreOfficeTimeout time.Duration
}

// DefaultConverterConfig returns the default converter configuration.
func DefaultConverterConfig() ConverterConfig {
	return ConverterConfig{
		DPI:                300,
		ImageFormat:        renderer.ImageFormatPNG,
		LibreOfficeTimeout: 300 * time.Second,
	}
}

// ConverterHandle provides access to shared services for converters.
// In-tree converters are stateless and
// do not use it, but out-of-tree converters may.
type ConverterHandle interface {
	Settings() config.Config
}

// SettingsHandle is a ConverterHandle backed by gateway settings.
type SettingsHandle struct {
	SettingsValue config.Config
}

// Settings returns the gateway settings.
func (handle SettingsHandle) Settings() config.Config { return handle.SettingsValue }

// ConverterFactory creates a DocumentConverter from static configuration.
type ConverterFactory func(ctx context.Context, configuration ConverterConfig, handle ConverterHandle) (DocumentConverter, error)

// Registry maps file extensions to their converter factories.
type Registry map[string]ConverterFactory

// Register adds a converter factory for the given extension, rejecting nil
// factories, invalid extensions, and duplicate extensions.
func (registry Registry) Register(extension string, factory ConverterFactory) error {
	normalized := strings.ToLower(extension)
	if normalized == "" || !strings.HasPrefix(normalized, ".") {
		return fmt.Errorf("invalid converter extension: %q", extension)
	}
	if factory == nil {
		return fmt.Errorf("converter factory must not be nil")
	}
	if _, exists := registry[normalized]; exists {
		return fmt.Errorf("converter already registered for %s", normalized)
	}
	registry[normalized] = factory
	return nil
}

// Unregister removes the converter factory for the given extension.
func (registry Registry) Unregister(extension string) error {
	normalized := strings.ToLower(extension)
	if _, exists := registry[normalized]; !exists {
		return fmt.Errorf("converter not registered for %s", normalized)
	}
	delete(registry, normalized)
	return nil
}

// Merge adds all converter factories from another registry, failing on
// conflicts.
func (registry Registry) Merge(in Registry) error {
	for extension, factory := range in {
		if _, exists := registry[extension]; exists {
			return fmt.Errorf("converter already registered for %s", extension)
		}
		registry[extension] = factory
	}
	return nil
}

// Converter returns the converter registered for the given extension,
// instantiating it through its factory.
func (registry Registry) Converter(ctx context.Context, extension string, configuration ConverterConfig, handle ConverterHandle) (DocumentConverter, error) {
	normalized := strings.ToLower(extension)
	factory, found := registry[normalized]
	if !found {
		if normalized == "" {
			normalized = "(none)"
		}
		return nil, fmt.Errorf("unsupported file type: %s", normalized)
	}
	return factory(ctx, configuration, handle)
}

// ForPath returns the converter for the extension of the given path.
func (registry Registry) ForPath(ctx context.Context, path string, configuration ConverterConfig, handle ConverterHandle) (DocumentConverter, error) {
	return registry.Converter(ctx, filepath.Ext(path), configuration, handle)
}

// Extensions returns all registered extensions in sorted order.
func (registry Registry) Extensions() []string {
	extensions := make([]string, 0, len(registry))
	for extension := range registry {
		extensions = append(extensions, extension)
	}
	sort.Strings(extensions)
	return extensions
}

// ConverterAdapter adapts a constructor into a ConverterFactory, reducing
// boilerplate for stateless in-tree converters.
func ConverterAdapter(construct func(config ConverterConfig) DocumentConverter) ConverterFactory {
	return func(_ context.Context, configuration ConverterConfig, _ ConverterHandle) (DocumentConverter, error) {
		return construct(configuration), nil
	}
}

// NewInTreeRegistry returns the registry with all built-in converters
// (PDF, Office, text, and image formats).
func NewInTreeRegistry() Registry {
	return Registry{
		".pdf":  ConverterAdapter(func(config ConverterConfig) DocumentConverter { return pdfConverter{config: config} }),
		".doc":  ConverterAdapter(func(config ConverterConfig) DocumentConverter { return docConverter{config: config} }),
		".docx": ConverterAdapter(func(config ConverterConfig) DocumentConverter { return docxConverter{config: config} }),
		".ppt":  ConverterAdapter(func(config ConverterConfig) DocumentConverter { return pptConverter{config: config} }),
		".pptx": ConverterAdapter(func(config ConverterConfig) DocumentConverter { return pptxConverter{config: config} }),
		".xls":  ConverterAdapter(func(config ConverterConfig) DocumentConverter { return xlsConverter{config: config} }),
		".xlsx": ConverterAdapter(func(config ConverterConfig) DocumentConverter { return xlsxConverter{config: config} }),
		".xlsm": ConverterAdapter(func(config ConverterConfig) DocumentConverter { return xlsmConverter{config: config} }),
		".txt": ConverterAdapter(func(ConverterConfig) DocumentConverter {
			return newTextConverter(".txt", "text/plain", extractor.PlainText)
		}),
		".md": ConverterAdapter(func(ConverterConfig) DocumentConverter {
			return newTextConverter(".md", "text/markdown", extractor.PlainText)
		}),
		".markdown": ConverterAdapter(func(ConverterConfig) DocumentConverter {
			return newTextConverter(".markdown", "text/markdown", extractor.PlainText)
		}),
		".csv":  ConverterAdapter(func(ConverterConfig) DocumentConverter { return newTextConverter(".csv", "text/csv", extractor.CSV) }),
		".html": ConverterAdapter(func(ConverterConfig) DocumentConverter { return newTextConverter(".html", "text/html", extractor.HTML) }),
		".htm":  ConverterAdapter(func(ConverterConfig) DocumentConverter { return newTextConverter(".htm", "text/html", extractor.HTML) }),
		".jpg":  ConverterAdapter(func(ConverterConfig) DocumentConverter { return jpgConverter{} }),
		".jpeg": ConverterAdapter(func(ConverterConfig) DocumentConverter { return jpegConverter{} }),
		".png":  ConverterAdapter(func(ConverterConfig) DocumentConverter { return pngConverter{} }),
	}
}
