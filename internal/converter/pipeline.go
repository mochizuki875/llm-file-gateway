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
	"time"

	"github.com/mochizuki875/document-image-renderer/pkg/renderer"
)

func convertRenderedDocument(ctx context.Context, source, outputDir, mediaType string, options Options) (Result, error) {
	var textBlocks []string
	if !options.DisableTextExtraction {
		extractOptions := renderer.DefaultExtractOptions()
		extractOptions.LibreOfficeTimeout = 300 * time.Second
		extracted, err := renderer.ExtractDocumentWithOptions(ctx, source, &extractOptions)
		if err != nil {
			return Result{}, err
		}
		textBlocks = make([]string, 0, len(extracted.Parts))
		for _, part := range extracted.Parts {
			textBlocks = append(textBlocks, part.Text)
		}
		if err := validateTextLimit(textBlocks, options.MaxTextChars); err != nil {
			return Result{}, err
		}
	}
	return withOutputDirectory(outputDir, func() (Result, error) {
		renderOptions := renderer.DefaultRenderOptions()
		renderOptions.DPI = 150
		renderOptions.ImageFormat = renderer.ImageFormatPNG
		renderOptions.LibreOfficeTimeout = 300 * time.Second
		rendered, err := renderer.RenderDocument(ctx, source, outputDir, &renderOptions)
		if err != nil {
			return Result{}, err
		}
		if rendered.PageCount() > options.MaxPages {
			return Result{}, &PageLimitError{Limit: options.MaxPages}
		}
		artifacts := make([]Artifact, 0, len(rendered.Images))
		for index, page := range rendered.Images {
			text := ""
			if index < len(textBlocks) {
				text = textBlocks[index]
			}
			textName := fmt.Sprintf("page-%04d.txt", index+1)
			if err := os.WriteFile(filepath.Join(outputDir, textName), []byte(text), 0o644); err != nil {
				return Result{}, err
			}
			imageName := filepath.Base(page.Path)
			digest, err := hashFile(page.Path)
			if err != nil {
				return Result{}, err
			}
			pageNumber, width, height := page.PageNumber, page.Width, page.Height
			imageMediaType := "image/png"
			artifacts = append(artifacts, Artifact{
				PartNumber: index + 1, PageNumber: &pageNumber, TextPath: textName,
				ImagePath: &imageName, Width: &width, Height: &height,
				MediaType: &imageMediaType, SHA256: &digest,
			})
		}
		return writeResult(source, outputDir, mediaType, artifacts)
	})
}

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
		return writeResult(source, outputDir, mediaType, artifacts)
	})
}

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
		return writeResult(source, outputDir, mediaType, []Artifact{artifact})
	})
}

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

func writeResult(source, outputDir, mediaType string, artifacts []Artifact) (Result, error) {
	digest, err := hashFile(source)
	if err != nil {
		return Result{}, err
	}
	warnings := []string{}
	manifest := Manifest{
		SchemaVersion: 2, ConverterVersion: "2026.09.0",
		Source:    ManifestSource{MediaType: mediaType, SHA256: digest},
		Documents: []ManifestDocument{{Name: filepath.Base(source), Parts: artifacts}},
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

func imageSize(source string) (image.Config, string, error) {
	input, err := os.Open(source)
	if err != nil {
		return image.Config{}, "", err
	}
	defer input.Close()
	return image.DecodeConfig(input)
}

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
