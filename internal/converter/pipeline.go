package converter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mochizuki875/document-image-renderer/pkg/renderer"
)

// convertRenderedDocument converts PDF and Office documents by extracting text
// (unless disabled) and rendering each page to a PNG image via the
// document-image-renderer library.
func convertRenderedDocument(ctx context.Context, source, outputDir, mediaType string, config ConverterConfig, options Options) (Result, error) {
	documentText := ""
	if !options.DisableTextExtraction {
		extractOptions := renderer.DefaultExtractOptions()
		extractOptions.LibreOfficeTimeout = config.LibreOfficeTimeout
		extracted, err := renderer.ExtractDocumentWithOptions(ctx, source, &extractOptions)
		if err != nil {
			return Result{}, err
		}
		documentText = extracted.Text()
		if err := validateTextLimit([]string{documentText}, options.MaxTextChars); err != nil {
			return Result{}, err
		}
	}
	return withOutputDirectory(outputDir, func() (Result, error) {
		renderOptions := renderer.DefaultRenderOptions()
		renderOptions.DPI = config.DPI
		renderOptions.ImageFormat = config.ImageFormat
		renderOptions.LibreOfficeTimeout = config.LibreOfficeTimeout
		rendered, err := renderer.RenderDocument(ctx, source, outputDir, &renderOptions)
		if err != nil {
			return Result{}, err
		}
		if rendered.PageCount() > options.MaxPages {
			return Result{}, &PageLimitError{Limit: options.MaxPages}
		}
		textPath := ""
		if !options.DisableTextExtraction {
			textPath = "document.txt"
			if err := os.WriteFile(filepath.Join(outputDir, textPath), []byte(documentText), 0o644); err != nil {
				return Result{}, err
			}
		}
		artifacts := make([]Artifact, 0, len(rendered.Images))
		for index, page := range rendered.Images {
			imageName := filepath.Base(page.Path)
			digest, err := hashFile(page.Path)
			if err != nil {
				return Result{}, err
			}
			pageNumber, width, height := page.PageNumber, page.Width, page.Height
			imageMediaType := "image/png"
			artifacts = append(artifacts, Artifact{
				PartNumber: index + 1, PageNumber: &pageNumber,
				ImagePath: &imageName, Width: &width, Height: &height,
				MediaType: &imageMediaType, SHA256: &digest,
			})
		}
		return writeResult(source, outputDir, mediaType, textPath, artifacts)
	})
}

// convertTextDocument writes each text block to a part file and produces a
// manifest with no images.
func convertTextDocument(source, outputDir, mediaType string, textBlocks []string, options Options) (Result, error) {
	if err := validateTextLimit(textBlocks, options.MaxTextChars); err != nil {
		return Result{}, err
	}
	return withOutputDirectory(outputDir, func() (Result, error) {
		artifacts := make([]Artifact, 0, len(textBlocks))
		for index, text := range textBlocks {
			name := fmt.Sprintf("part-%04d.txt", index+1)
			if err := os.WriteFile(filepath.Join(outputDir, name), []byte(text), 0o644); err != nil {
				return Result{}, err
			}
			artifacts = append(artifacts, Artifact{PartNumber: index + 1, TextPath: name})
		}
		return writeResult(source, outputDir, mediaType, "", artifacts)
	})
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

// convertImageDocument copies an image into the output directory and records
// its dimensions in the manifest.
func convertImageDocument(source, outputDir, mediaType string, width, height int) (Result, error) {
	return withOutputDirectory(outputDir, func() (Result, error) {
		imageName := "image-0001" + strings.ToLower(filepath.Ext(source))
		if err := copyFile(source, filepath.Join(outputDir, imageName)); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(outputDir, "part-0001.txt"), nil, 0o644); err != nil {
			return Result{}, err
		}
		digest, err := hashFile(filepath.Join(outputDir, imageName))
		if err != nil {
			return Result{}, err
		}
		artifact := Artifact{
			PartNumber: 1, TextPath: "part-0001.txt", ImagePath: &imageName,
			Width: &width, Height: &height, MediaType: &mediaType, SHA256: &digest,
		}
		return writeResult(source, outputDir, mediaType, "", []Artifact{artifact})
	})
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
