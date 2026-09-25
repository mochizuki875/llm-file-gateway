package converter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mochizuki875/document-image-renderer/pkg/renderer"
)

// convertRenderedDocument converts PDF and Office documents by rendering each
// page to a PNG image via the document-image-renderer library and extracting
// text (unless disabled).
//
// Rendering runs before text extraction so that the page limit is enforced
// before the (potentially expensive) full-document text extraction: the
// renderer checks MaxPages before rendering any page, so an over-limit
// document fails fast without extracting text from every page. The character
// limit is delegated to the renderer via ExtractOptions.MaxCharacters.
func convertRenderedDocument(ctx context.Context, source, outputDir, mediaType string, config ConverterConfig, options Options) (Result, error) {
	return withOutputDirectory(outputDir, func() (Result, error) {
		renderOptions := renderer.DefaultRenderOptions()
		renderOptions.DPI = config.DPI
		renderOptions.ImageFormat = config.ImageFormat
		renderOptions.LibreOfficeTimeout = config.LibreOfficeTimeout
		renderOptions.MaxPages = options.MaxPages
		rendered, err := renderer.RenderDocument(ctx, source, outputDir, &renderOptions)
		if err != nil {
			return Result{}, mapRendererError(err, options.MaxPages)
		}
		documentText := ""
		if !options.DisableTextExtraction {
			extractOptions := renderer.DefaultExtractOptions()
			extractOptions.LibreOfficeTimeout = config.LibreOfficeTimeout
			extractOptions.MaxCharacters = options.MaxTextChars
			extracted, err := renderer.ExtractDocumentWithOptions(ctx, source, &extractOptions)
			if err != nil {
				return Result{}, mapRendererError(err, options.MaxPages)
			}
			documentText = extracted.Text()
		}
		textPath := ""
		if !options.DisableTextExtraction {
			textPath = "document.txt"
			if err := os.WriteFile(filepath.Join(outputDir, textPath), []byte(documentText), 0o600); err != nil {
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

// mapRendererError converts renderer errors into converter errors. In
// particular, a page-limit exceeded error from the renderer is mapped to a
// PageLimitError and a character-limit exceeded error is mapped to a
// TextLimitError so that callers can distinguish them from transient
// failures.
func mapRendererError(err error, maxPages int) error {
	var pageLimitError *renderer.PageLimitExceededError
	if errors.As(err, &pageLimitError) {
		return &PageLimitError{Limit: maxPages}
	}
	var characterLimitError *renderer.CharacterLimitExceededError
	if errors.As(err, &characterLimitError) {
		return &TextLimitError{Limit: characterLimitError.MaxCharacters}
	}
	return err
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
			if err := os.WriteFile(filepath.Join(outputDir, name), []byte(text), 0o600); err != nil {
				return Result{}, err
			}
			artifacts = append(artifacts, Artifact{PartNumber: index + 1, TextPath: name})
		}
		return writeResult(source, outputDir, mediaType, "", artifacts)
	})
}

// convertImageDocument copies an image into the output directory and records
// its dimensions in the manifest.
func convertImageDocument(source, outputDir, mediaType string, width, height int) (Result, error) {
	return withOutputDirectory(outputDir, func() (Result, error) {
		imageName := "image-0001" + strings.ToLower(filepath.Ext(source))
		if err := copyFile(source, filepath.Join(outputDir, imageName)); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(filepath.Join(outputDir, "part-0001.txt"), nil, 0o600); err != nil {
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
