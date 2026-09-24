package converter

import "context"

// xlsxConverter converts Excel Open XML workbooks.
type xlsxConverter struct {
	config ConverterConfig
}

func (xlsxConverter) Extension() string { return ".xlsx" }
func (xlsxConverter) MediaType() string {
	return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
}
func (xlsxConverter) Validate(source string) error { return validateSignature(source, zipSignature) }
func (documentConverter xlsxConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), documentConverter.config, options)
}
