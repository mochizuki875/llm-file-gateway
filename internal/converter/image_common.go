package converter

import (
	"errors"
	_ "image/jpeg"
	_ "image/png"
)

// validateImage checks that the file at source is a valid image of the given
// format.
func validateImage(source, format string) error {
	_, detected, err := imageSize(source)
	if err != nil {
		return errors.New("file is not a valid image")
	}
	if detected != format {
		return errors.New("file content does not match its extension")
	}
	return nil
}

// convertImage converts an image file into a single-part document.
func convertImage(source, outputDir, mediaType string) (Result, error) {
	config, _, err := imageSize(source)
	if err != nil {
		return Result{}, err
	}
	return convertImageDocument(source, outputDir, mediaType, config.Width, config.Height)
}
