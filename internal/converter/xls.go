package converter

import "context"

type xlsConverter struct{}

func (xlsConverter) Extension() string { return ".xls" }
func (xlsConverter) MediaType() string { return "application/vnd.ms-excel" }
func (xlsConverter) Validate(source string) error {
	return validateSignature(source, oleSignature)
}
func (documentConverter xlsConverter) Convert(ctx context.Context, source, outputDir string, options Options) (Result, error) {
	return convertRenderedDocument(ctx, source, outputDir, documentConverter.MediaType(), options)
}
