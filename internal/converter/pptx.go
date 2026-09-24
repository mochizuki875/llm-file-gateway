package converter

import "context"

// pptxConverter converts PowerPoint Open XML presentations.
type pptxConverter struct {
	config ConverterConfig
}

func (pptxConverter) Extension() string { return ".pptx" }
func (pptxConverter) MediaType() string {
	return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
}
func (pptxConverter) Validate(source string) error { return validateSignature(source, zipSignature) }
func (documentConverter pptxConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), documentConverter.config, options)
}
