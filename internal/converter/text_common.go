package converter

import (
	"context"

	"github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"
)

type textConverter struct {
	extension string
	mediaType string
	extract   extractor.Extractor
}

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

func validateExtractedText(source string, extract extractor.Extractor) error {
	_, err := extract(source)
	return err
}

func convertExtractedText(source, outputDir, mediaType string, extract extractor.Extractor, options Options) (Result, error) {
	text, err := extract(source)
	if err != nil {
		return Result{}, err
	}
	return convertTextDocument(source, outputDir, mediaType, []string{text}, options)
}
