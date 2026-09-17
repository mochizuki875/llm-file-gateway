package converter

import "context"

type pdfConverter struct{}

func (pdfConverter) Extension() string { return ".pdf" }
func (pdfConverter) MediaType() string { return "application/pdf" }
func (pdfConverter) Validate(source string) error {
	return validateSignature(source, []byte("%PDF-"))
}
func (documentConverter pdfConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), options)
}
