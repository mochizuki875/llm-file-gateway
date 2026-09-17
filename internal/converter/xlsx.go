package converter

import "context"

type xlsxConverter struct{}

func (xlsxConverter) Extension() string { return ".xlsx" }
func (xlsxConverter) MediaType() string {
	return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
}
func (xlsxConverter) Validate(source string) error { return validateSignature(source, zipSignature) }
func (documentConverter xlsxConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), options)
}
