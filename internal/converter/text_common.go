package converter

import (
	"context"

	"github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"
)

// textConverter converts text-like files using an extractor.
type textConverter struct {
	extension string
	mediaType string
	extract   extractor.Extractor
}

// newTextConverter creates a text converter for the given extension and media
// type.
func newTextConverter(extension, mediaType string, extract extractor.Extractor) textConverter {
	return textConverter{extension: extension, mediaType: mediaType, extract: extract}
}

func (documentConverter textConverter) Extension() string {
	return documentConverter.extension
}

func (documentConverter textConverter) MediaType() string {
	return documentConverter.mediaType
}

func (documentConverter textConverter) Validate(source string) error {
	return validateExtractedText(source, documentConverter.extract)
}

func (documentConverter textConverter) Convert(_ context.Context, source, outputDir string, options Options) (Result, error) {
	return convertExtractedText(source, outputDir, documentConverter.mediaType, documentConverter.extract, options)
}

// validateExtractedText runs the extractor once to validate that the source
// can be extracted.
func validateExtractedText(source string, extract extractor.Extractor) error {
	_, err := extract(source)
	return err
}

// convertExtractedText extracts text from the source and converts it into a
// text-only document.
func convertExtractedText(source, outputDir, mediaType string, extract extractor.Extractor, options Options) (Result, error) {
	text, err := extract(source)
	if err != nil {
		return Result{}, err
	}
	return convertTextDocument(source, outputDir, mediaType, []string{text}, options)
}
