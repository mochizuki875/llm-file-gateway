package converter

import "context"

// pptConverter converts legacy PowerPoint documents.
type pptConverter struct {
	config ConverterConfig
}

func (pptConverter) Extension() string { return ".ppt" }
func (pptConverter) MediaType() string { return "application/vnd.ms-powerpoint" }
func (pptConverter) Validate(source string) error {
	return validateSignature(source, oleSignature)
}
func (documentConverter pptConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), documentConverter.config, options)
}
