package converter

import "context"

// jpgConverter converts JPEG images.
type jpgConverter struct{}

func (jpgConverter) Extension() string { return ".jpg" }
func (jpgConverter) MediaType() string { return "image/jpeg" }
func (jpgConverter) Validate(source string) error {
	return validateImage(source, "jpeg")
}
func (documentConverter jpgConverter) Convert(_ context.Context, source, outputDir string, _ Options) (Result, error) {
	return convertImage(source, outputDir, documentConverter.MediaType())
}
