package converter

import "context"

// jpegConverter converts JPEG images.
type jpegConverter struct{}

func (jpegConverter) Extension() string { return ".jpeg" }
func (jpegConverter) MediaType() string { return "image/jpeg" }
func (jpegConverter) Validate(source string) error {
	return validateImage(source, "jpeg")
}
func (documentConverter jpegConverter) Convert(_ context.Context, source, outputDir string, _ Options) (Result, error) {
	return convertImage(source, outputDir, documentConverter.MediaType())
}
