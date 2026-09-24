package converter

import "context"

// docxConverter converts Word Open XML documents.
type docxConverter struct {
	config ConverterConfig
}

func (docxConverter) Extension() string { return ".docx" }
func (docxConverter) MediaType() string {
	return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
}
func (docxConverter) Validate(source string) error { return validateSignature(source, zipSignature) }
func (documentConverter docxConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), documentConverter.config, options)
}
