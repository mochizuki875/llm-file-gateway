package converter

import (
	"errors"
	_ "image/jpeg"
	_ "image/png"
)

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

func convertImage(source, outputDir, mediaType string) (Result, error) {
	config, _, err := imageSize(source)
	if err != nil {
		return Result{}, err
	}
	return convertImageDocument(source, outputDir, mediaType, config.Width, config.Height)
}
