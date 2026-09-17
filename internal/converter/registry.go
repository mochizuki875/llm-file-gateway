package converter

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
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

var defaultRegistry = mustRegistry(
	pdfConverter{},
	docConverter{},
	docxConverter{},
	pptConverter{},
	pptxConverter{},
	xlsConverter{},
	xlsxConverter{},
	xlsmConverter{},
	newTextConverter(".txt", "text/plain", extractPlainText),
	newTextConverter(".md", "text/markdown", extractPlainText),
	newTextConverter(".markdown", "text/markdown", extractPlainText),
	newTextConverter(".csv", "text/csv", extractCSVText),
	newTextConverter(".html", "text/html", extractHTMLText),
	newTextConverter(".htm", "text/html", extractHTMLText),
	jpgConverter{},
	jpegConverter{},
	pngConverter{},
)

func mustRegistry(converters ...DocumentConverter) *Registry {
	registry, err := NewRegistry(converters...)
	if err != nil {
		panic(err)
	}
	return registry
}
