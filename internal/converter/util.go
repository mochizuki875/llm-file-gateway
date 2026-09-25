package converter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"io"
	"os"
	"path/filepath"
)

// imageSize decodes the image header of source and returns its config and
// detected format.
func imageSize(source string) (image.Config, string, error) {
	input, err := os.Open(source)
	if err != nil {
		return image.Config{}, "", err
	}
	defer input.Close()
	return image.DecodeConfig(input)
}

// hashFile returns the hex-encoded SHA-256 digest of the file at path.
func hashFile(path string) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer input.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, input); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// copyFile copies the contents of source to destination.
func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

// validateTextLimit returns a TextLimitError when the combined rune count of
// all text blocks exceeds limit.
func validateTextLimit(textBlocks []string, limit int) error {
	textCharacters := 0
	for _, text := range textBlocks {
		textCharacters += len([]rune(text))
	}
	if textCharacters > limit {
		return &TextLimitError{Limit: limit}
	}
	return nil
}

// withOutputDirectory prepares a clean output directory, runs convert, and
// removes the output again when conversion fails.
func withOutputDirectory(outputDir string, convert func() (Result, error)) (result Result, err error) {
	if err := removeConversionOutput(outputDir); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return Result{}, err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = removeConversionOutput(outputDir)
		}
	}()
	result, err = convert()
	if err != nil {
		return Result{}, err
	}
	succeeded = true
	return result, nil
}

// removeConversionOutput deletes the output directory and any stale manifest
// from a previous conversion.
func removeConversionOutput(outputDir string) error {
	if err := os.RemoveAll(outputDir); err != nil {
		return err
	}
	manifestPath := filepath.Join(filepath.Dir(outputDir), "manifest.json")
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeResult writes manifest.json next to the output directory and returns
// the conversion result.
func writeResult(source, outputDir, mediaType, textPath string, artifacts []Artifact) (Result, error) {
	digest, err := hashFile(source)
	if err != nil {
		return Result{}, err
	}
	warnings := []string{}
	manifest := Manifest{
		SchemaVersion: 3, ConverterVersion: "2026.09.0",
		Source:    ManifestSource{MediaType: mediaType, SHA256: digest},
		Documents: []ManifestDocument{{Name: filepath.Base(source), TextPath: textPath, Parts: artifacts}},
		Warnings:  warnings,
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Result{}, err
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(filepath.Dir(outputDir), "manifest.json"), encoded, 0o644); err != nil {
		return Result{}, err
	}
	return Result{Manifest: manifest, Artifacts: artifacts, Warnings: warnings}, nil
}
