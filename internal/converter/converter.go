package converter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
		renderOptions.RenderTimeout = config.RenderTimeout
		renderOptions.LibreOfficeTimeout = config.LibreOfficeTimeout
		renderOptions.MaxPages = options.MaxPages
		renderOptions.MaxPDFBytes = config.MaxPDFBytes
		renderOptions.MaxPageWidth = config.MaxPageWidth
		renderOptions.MaxPageHeight = config.MaxPageHeight
		renderOptions.MaxPagePixels = config.MaxPagePixels
		renderOptions.MaxDocumentPixels = config.MaxDocumentPixels
		renderOptions.TargetPagePixels = config.TargetPagePixels
		renderOptions.MaxOOXMLMembers = config.MaxOOXMLMembers
		renderOptions.MaxOOXMLMemberBytes = config.MaxOOXMLMemberBytes
		renderOptions.MaxOOXMLTotalBytes = config.MaxOOXMLTotalBytes
		rendered, err := renderer.RenderDocument(ctx, source, outputDir, &renderOptions)
		if err != nil {
			return Result{}, mapRendererError(err, options.MaxPages)
		}
		var extracted *renderer.ExtractResult
		if !options.DisableTextExtraction {
			extractOptions := renderer.DefaultExtractOptions()
			extractOptions.LibreOfficeTimeout = config.LibreOfficeTimeout
			extractOptions.MaxCharacters = options.MaxTextChars
			extractOptions.MaxPDFBytes = config.MaxPDFBytes
			extractOptions.MaxOOXMLMembers = config.MaxOOXMLMembers
			extractOptions.MaxOOXMLMemberBytes = config.MaxOOXMLMemberBytes
			extractOptions.MaxOOXMLTotalBytes = config.MaxOOXMLTotalBytes
			extracted, err = renderer.ExtractDocumentWithOptions(ctx, source, &extractOptions)
			if err != nil {
				return Result{}, mapRendererError(err, options.MaxPages)
			}
		}
		artifacts := make([]ManifestPart, 0, len(rendered.Images))
		warnings := make([]string, 0)
		for index, page := range rendered.Images {
			imageName := filepath.Base(page.Path)
			textPath := ""
			if extracted != nil {
				textPath = strings.TrimSuffix(imageName, filepath.Ext(imageName)) + ".txt"
				text := ""
				if part, found := extracted.Part(page.PageNumber); found {
					text = part.Text
				}
				if err := os.WriteFile(filepath.Join(outputDir, textPath), []byte(text), 0o600); err != nil {
					return Result{}, err
				}
			}
			digest, err := hashFile(page.Path)
			if err != nil {
				return Result{}, err
			}
			pageNumber, width, height := page.PageNumber, page.Width, page.Height
			imageMediaType := "image/png"
			if config.ImageFormat == renderer.ImageFormatJPEG {
				imageMediaType = "image/jpeg"
			}
			artifacts = append(artifacts, ManifestPart{
				PartNumber: index + 1, PageNumber: &pageNumber,
				TextPath: textPath, ImagePath: &imageName, Width: &width, Height: &height,
				MediaType: &imageMediaType, SHA256: &digest,
			})
			if page.Scaled {
				warning := fmt.Sprintf("image_scaled: part=%d requested_width=%d requested_height=%d width=%d height=%d target_pixels=%d", index+1, page.RequestedWidth, page.RequestedHeight, page.Width, page.Height, config.TargetPagePixels)
				warnings = append(warnings, warning)
				logImageScaled(source, options, config, index+1, page)
			}
		}
		return writeResult(source, outputDir, mediaType, artifacts, warnings)
	})
}

func logImageScaled(source string, options Options, config ConverterConfig, part int, image renderer.RenderedImage) {
	filename := options.Filename
	if filename == "" {
		filename = filepath.Base(source)
	}
	attributes := []any{
		"event", "image_scaled",
		"filename", filename,
	}
	if options.FileID != "" {
		attributes = append(attributes, "file_id", options.FileID)
	}
	attributes = append(attributes,
		"part", part,
		"requested_width", image.RequestedWidth,
		"requested_height", image.RequestedHeight,
		"width", image.Width,
		"height", image.Height,
		"dpi", config.DPI,
		"target_pixels", config.TargetPagePixels,
	)
	slog.Info("document image scaled", attributes...)
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
	var pageSizeLimitError *renderer.PageSizeLimitExceededError
	if errors.As(err, &pageSizeLimitError) {
		return &ImageLimitError{Err: pageSizeLimitError}
	}
	var documentPixelLimitError *renderer.DocumentPixelLimitExceededError
	if errors.As(err, &documentPixelLimitError) {
		return &ImageLimitError{Err: documentPixelLimitError}
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
		artifacts := make([]ManifestPart, 0, len(textBlocks))
		for index, text := range textBlocks {
			name := fmt.Sprintf("part-%04d.txt", index+1)
			if err := os.WriteFile(filepath.Join(outputDir, name), []byte(text), 0o600); err != nil {
				return Result{}, err
			}
			artifacts = append(artifacts, ManifestPart{PartNumber: index + 1, TextPath: name})
		}
		return writeResult(source, outputDir, mediaType, artifacts, []string{})
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
		artifact := ManifestPart{
			PartNumber: 1, TextPath: "part-0001.txt", ImagePath: &imageName,
			Width: &width, Height: &height, MediaType: &mediaType, SHA256: &digest,
		}
		return writeResult(source, outputDir, mediaType, []ManifestPart{artifact}, []string{})
	})
}
