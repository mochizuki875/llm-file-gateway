package converter

import "context"

// xlsmConverter converts macro-enabled Excel workbooks.
type xlsmConverter struct {
	config ConverterConfig
}

func (xlsmConverter) Extension() string { return ".xlsm" }
func (xlsmConverter) MediaType() string {
	return "application/vnd.ms-excel.sheet.macroEnabled.12"
}
func (xlsmConverter) Validate(source string) error { return validateSignature(source, zipSignature) }
func (documentConverter xlsmConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), documentConverter.config, options)
}
