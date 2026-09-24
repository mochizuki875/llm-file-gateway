package converter

import "context"

// docConverter converts legacy Word documents.
type docConverter struct {
	config ConverterConfig
}

func (docConverter) Extension() string            { return ".doc" }
func (docConverter) MediaType() string            { return "application/msword" }
func (docConverter) Validate(source string) error { return validateSignature(source, oleSignature) }
func (documentConverter docConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), documentConverter.config, options)
}
