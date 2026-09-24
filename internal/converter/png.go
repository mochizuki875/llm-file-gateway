package converter

import "context"

// pngConverter converts PNG images.
type pngConverter struct{}

func (pngConverter) Extension() string { return ".png" }
func (pngConverter) MediaType() string { return "image/png" }
func (pngConverter) Validate(source string) error {
	return validateImage(source, "png")
}
func (documentConverter pngConverter) Convert(_ context.Context, source, outputDir string, _ Options) (Result, error) {
	return convertImage(source, outputDir, documentConverter.MediaType())
}
