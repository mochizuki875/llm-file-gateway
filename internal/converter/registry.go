package converter

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"
)

type Registry struct {
	byExtension map[string]DocumentConverter
}

func NewRegistry(converters ...DocumentConverter) (*Registry, error) {
	registry := &Registry{byExtension: make(map[string]DocumentConverter)}
	for _, documentConverter := range converters {
		if documentConverter == nil {
			return nil, fmt.Errorf("converter must not be nil")
		}
		extension := documentConverter.Extension()
		normalized := strings.ToLower(extension)
		if normalized == "" || !strings.HasPrefix(normalized, ".") {
			return nil, fmt.Errorf("invalid converter extension: %q", extension)
		}
		if _, exists := registry.byExtension[normalized]; exists {
			return nil, fmt.Errorf("converter already registered for %s", normalized)
		}
		registry.byExtension[normalized] = documentConverter
	}
	return registry, nil
}

func (registry *Registry) Converter(extension string) (DocumentConverter, error) {
	normalized := strings.ToLower(extension)
	documentConverter, found := registry.byExtension[normalized]
	if !found {
		if normalized == "" {
			normalized = "(none)"
		}
		return nil, fmt.Errorf("unsupported file type: %s", normalized)
	}
	return documentConverter, nil
}

func (registry *Registry) ForPath(path string) (DocumentConverter, error) {
	return registry.Converter(filepath.Ext(path))
}

func (registry *Registry) Extensions() []string {
	extensions := make([]string, 0, len(registry.byExtension))
	for extension := range registry.byExtension {
		extensions = append(extensions, extension)
	}
	sort.Strings(extensions)
	return extensions
}

func NewDefaultRegistry() (*Registry, error) {
	return NewRegistry(
		pdfConverter{},
		docConverter{},
		docxConverter{},
		pptConverter{},
		pptxConverter{},
		xlsConverter{},
		xlsxConverter{},
		xlsmConverter{},
		newTextConverter(".txt", "text/plain", extractor.PlainText),
		newTextConverter(".md", "text/markdown", extractor.PlainText),
		newTextConverter(".markdown", "text/markdown", extractor.PlainText),
		newTextConverter(".csv", "text/csv", extractor.CSV),
		newTextConverter(".html", "text/html", extractor.HTML),
		newTextConverter(".htm", "text/html", extractor.HTML),
		jpgConverter{},
		jpegConverter{},
		pngConverter{},
	)
}
