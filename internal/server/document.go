package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
)

// resolvedDocument is a document that has been resolved to its conversion
// artifacts, either from an uploaded file or from inline data.
type resolvedDocument struct {
	filename   string
	derivedDir string
	manifest   converter.Manifest
	// release is called when the caller is done reading the artifacts. It is
	// nil for inline documents, whose temporary directory is cleaned up by
	// the request handler.
	release func()
}

// prepareDocument resolves a file reference (file_id, file_data, or file_url)
// into a converted document. Inline data is written to a temporary directory
// that is cleaned up after the request completes.
func (server *Server) prepareDocument(ctx context.Context, reference map[string]any, tenantID, param string, temporary *[]string) (resolvedDocument, error) {
	if err := ctx.Err(); err != nil {
		return resolvedDocument{}, err
	}
	sources := 0
	for _, key := range []string{"file_id", "file_data", "file_url"} {
		if value, ok := reference[key].(string); ok && value != "" {
			sources++
		}
	}
	if sources != 1 {
		return resolvedDocument{}, apierror.New(400, "invalid_file_reference", "Specify exactly one file source.", param)
	}
	if id, ok := reference["file_id"].(string); ok && id != "" {
		record, manifest, release, err := server.files.Resolve(ctx, id, tenantID, param+".file_id")
		if err != nil {
			return resolvedDocument{}, err
		}
		return resolvedDocument{filename: record.Filename, derivedDir: filepath.Join(server.settings.DataDir, filepath.Dir(record.SourcePath), "derived"), manifest: manifest, release: release}, nil
	}
	filename, _ := reference["filename"].(string)
	var content []byte
	var err error
	if encoded, ok := reference["file_data"].(string); ok && encoded != "" {
		if filename == "" {
			return resolvedDocument{}, apierror.New(400, "invalid_file_data", "filename is required for file_data.", param+".filename")
		}
		if strings.HasPrefix(encoded, "data:") {
			_, encoded, _ = strings.Cut(encoded, ",")
		}
		content, err = base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil {
			return resolvedDocument{}, apierror.New(400, "invalid_file_data", "file_data is not valid base64.", param+".file_data")
		}
	} else {
		content, filename, err = server.downloadPublicHTTPS(ctx, reference["file_url"].(string), param)
		if err != nil {
			return resolvedDocument{}, err
		}
		if override, ok := reference["filename"].(string); ok && override != "" {
			filename = override
		}
	}
	if err := ctx.Err(); err != nil {
		return resolvedDocument{}, err
	}
	if server.settings.MaxFileBytes > 0 && int64(len(content)) > server.settings.MaxFileBytes {
		return resolvedDocument{}, apierror.FileTooLarge(server.settings.MaxFileBytes, param)
	}
	directory, err := createRequestDirectory(filepath.Join(server.settings.DataDir, "work"))
	if err != nil {
		return resolvedDocument{}, err
	}
	*temporary = append(*temporary, directory)
	source := filepath.Join(directory, "source"+strings.ToLower(filepath.Ext(filename)))
	if err := os.WriteFile(source, content, 0o600); err != nil {
		return resolvedDocument{}, err
	}
	documentConverter, err := server.files.ResolveConverter(ctx, source)
	if err != nil {
		return resolvedDocument{}, err
	}
	if err := ctx.Err(); err != nil {
		return resolvedDocument{}, err
	}
	validationError := documentConverter.Validate(source)
	if err := ctx.Err(); err != nil {
		return resolvedDocument{}, err
	}
	if err := validationError; err != nil {
		return resolvedDocument{}, apierror.New(400, "file_processing_failed", err.Error(), param)
	}
	result, err := documentConverter.Convert(ctx, source, filepath.Join(directory, "derived"), converter.Options{
		MaxPages: server.settings.MaxDocumentPages, MaxTextChars: server.settings.MaxDocumentTextChars,
		DisableTextExtraction: !server.settings.TextExtractionEnabled,
		Filename:              filepath.Base(filename),
	})
	if err := ctx.Err(); err != nil {
		return resolvedDocument{}, err
	}
	if err != nil {
		slog.Error("inline document conversion failed", "filename", filepath.Base(filename), "error", err)
		var pageLimitError *converter.PageLimitError
		if errors.As(err, &pageLimitError) {
			return resolvedDocument{}, apierror.New(400, "too_many_pages", err.Error(), param)
		}
		var textLimitError *converter.TextLimitError
		if errors.As(err, &textLimitError) {
			return resolvedDocument{}, apierror.New(400, "document_text_too_large", err.Error(), param)
		}
		var imageLimitError *converter.ImageLimitError
		if errors.As(err, &imageLimitError) {
			return resolvedDocument{}, apierror.New(400, "image_too_large", err.Error(), param)
		}
		return resolvedDocument{}, apierror.New(400, "file_processing_failed", err.Error(), param)
	}
	return resolvedDocument{filename: filepath.Base(filename), derivedDir: filepath.Join(directory, "derived"), manifest: result.Manifest}, nil
}

func createRequestDirectory(workDir string) (string, error) {
	directory, err := os.MkdirTemp(workDir, "gateway-request-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", err
	}
	return directory, nil
}

// documentParts converts a resolved document into the content parts expected
// by the Responses API (kind="responses") or Chat Completions API
// (kind="chat"), including extracted text and base64 data URL images.
func (server *Server) documentParts(document resolvedDocument, kind, param string) ([]any, error) {
	if len(document.manifest.Documents) == 0 {
		return nil, errors.New("document manifest contains no documents")
	}
	textType := "input_text"
	if kind == "chat" {
		textType = "text"
	}
	metadata := fmt.Sprintf(`<document_metadata filename="%s"`, html.EscapeString(document.filename))
	if mediaType := document.manifest.Source.MediaType; mediaType != "" {
		metadata += fmt.Sprintf(` media_type="%s"`, html.EscapeString(mediaType))
	}
	parts := []any{map[string]any{"type": textType, "text": metadata + " />"}}
	manifestDocument := document.manifest.Documents[0]
	images := 0
	var documentPixels uint64
	validateRenderedImages := isRenderedDocumentMediaType(document.manifest.Source.MediaType)
	for _, artifact := range manifestDocument.Parts {
		if artifact.TextPath != "" {
			text, err := os.ReadFile(filepath.Join(document.derivedDir, artifact.TextPath))
			if err != nil {
				slog.Error("document text artifact read failed", "filename", document.filename, "part", artifact.PartNumber, "error", err)
				return nil, err
			}
			if len(text) > 0 {
				location := fmt.Sprintf("part=\"%d\"", artifact.PartNumber)
				if artifact.PageNumber != nil {
					location = fmt.Sprintf("page=\"%d\"", *artifact.PageNumber)
				}
				label := fmt.Sprintf("<document filename=\"%s\" %s>\n%s\n</document>", html.EscapeString(document.filename), location, text)
				parts = append(parts, map[string]any{"type": textType, "text": label})
			}
		}
		if artifact.ImagePath == nil || (server.settings.MaxDocumentPages > 0 && images >= server.settings.MaxDocumentPages) {
			continue
		}
		imagePath := filepath.Join(document.derivedDir, *artifact.ImagePath)
		if validateRenderedImages {
			pixels, err := server.validateRenderedImageArtifact(imagePath, artifact, param)
			if err != nil {
				return nil, err
			}
			if limit := server.settings.MaxDocumentPixels; limit > 0 && (documentPixels > limit || pixels > limit-documentPixels) {
				return nil, imageTooLarge(limit, param)
			}
			documentPixels += pixels
		}
		imageContent, err := os.ReadFile(imagePath)
		if err != nil {
			slog.Error("document image artifact read failed", "filename", document.filename, "part", artifact.PartNumber, "error", err)
			return nil, err
		}
		mediaType := "image/png"
		if artifact.MediaType != nil {
			mediaType = *artifact.MediaType
		}
		dataURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(imageContent)
		if kind == "responses" {
			parts = append(parts, map[string]any{"type": "input_image", "detail": "auto", "image_url": dataURL})
		} else {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}})
		}
		images++
	}
	return parts, nil
}

func (server *Server) validateRenderedImageArtifact(path string, artifact converter.ManifestPart, param string) (uint64, error) {
	input, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	imageConfig, format, decodeErr := image.DecodeConfig(input)
	closeErr := input.Close()
	if decodeErr != nil {
		return 0, decodeErr
	}
	if closeErr != nil {
		return 0, closeErr
	}
	if imageConfig.Width <= 0 || imageConfig.Height <= 0 {
		return 0, errors.New("document image artifact has invalid dimensions")
	}
	if artifact.Width != nil && *artifact.Width != imageConfig.Width {
		return 0, errors.New("document image artifact width does not match manifest")
	}
	if artifact.Height != nil && *artifact.Height != imageConfig.Height {
		return 0, errors.New("document image artifact height does not match manifest")
	}
	detectedMediaType := "image/" + format
	if format == "jpeg" {
		detectedMediaType = "image/jpeg"
	}
	if artifact.MediaType != nil && *artifact.MediaType != detectedMediaType {
		return 0, errors.New("document image artifact media type does not match manifest")
	}
	width, height := uint64(imageConfig.Width), uint64(imageConfig.Height)
	pixels := width * height
	if limit := server.settings.DocumentImageTargetPixels; limit > 0 && pixels > limit {
		return 0, imageTooLarge(limit, param)
	}
	if limit := server.settings.MaxDocumentPageWidth; limit > 0 && imageConfig.Width > limit {
		return 0, imageTooLarge(uint64(limit), param)
	}
	if limit := server.settings.MaxDocumentPageHeight; limit > 0 && imageConfig.Height > limit {
		return 0, imageTooLarge(uint64(limit), param)
	}
	if limit := server.settings.MaxDocumentPagePixels; limit > 0 && pixels > limit {
		return 0, imageTooLarge(limit, param)
	}
	return pixels, nil
}

func imageTooLarge(limit uint64, param string) *apierror.Error {
	return apierror.New(422, "image_too_large", fmt.Sprintf("Image artifact exceeds the configured limit of %d pixels.", limit), param)
}

func isRenderedDocumentMediaType(mediaType string) bool {
	switch mediaType {
	case "application/pdf",
		"application/msword",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"application/vnd.ms-powerpoint",
		"application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"application/vnd.ms-excel",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"application/vnd.ms-excel.sheet.macroEnabled.12":
		return true
	default:
		return false
	}
}
