package converter

import "context"

type docxConverter struct{}

func (docxConverter) Extension() string { return ".docx" }
func (docxConverter) MediaType() string {
	return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
}
func (docxConverter) Validate(source string) error { return validateSignature(source, zipSignature) }
func (documentConverter docxConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), options)
}
