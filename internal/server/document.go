package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mochizuki875/llm-file-gateway/internal/apierror"
	"github.com/mochizuki875/llm-file-gateway/internal/converter"
)

type resolvedDocument struct {
	filename   string
	derivedDir string
	manifest   converter.Manifest
}

func (server *Server) resolveDocument(ctx context.Context, reference map[string]any, tenantID, param string, temporary *[]string) (resolvedDocument, error) {
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
		record, manifest, err := server.files.Resolve(ctx, id, tenantID, param+".file_id")
		if err != nil {
			return resolvedDocument{}, err
		}
		return resolvedDocument{record.Filename, filepath.Join(server.settings.DataDir, filepath.Dir(record.SourcePath), "derived"), manifest}, nil
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
	if int64(len(content)) > server.settings.MaxFileBytes {
		return resolvedDocument{}, apierror.FileTooLarge(server.settings.MaxFileBytes, param)
	}
	directory, err := os.MkdirTemp(filepath.Join(server.settings.DataDir, "work"), "gateway-request-")
	if err != nil {
		return resolvedDocument{}, err
	}
	*temporary = append(*temporary, directory)
	source := filepath.Join(directory, "source"+strings.ToLower(filepath.Ext(filename)))
	if err := os.WriteFile(source, content, 0o600); err != nil {
		return resolvedDocument{}, err
	}
	result, err := converter.Convert(ctx, source, filepath.Join(directory, "derived"), converter.Options{
		MaxPages: server.settings.MaxDocumentPages, MaxTextChars: server.settings.MaxDocumentTextChars,
		DisableTextExtraction: !server.settings.TextExtractionEnabled,
	})
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
		return resolvedDocument{}, apierror.New(400, "file_processing_failed", err.Error(), param)
	}
	return resolvedDocument{filepath.Base(filename), filepath.Join(directory, "derived"), result.Manifest}, nil
}

func (server *Server) documentParts(document resolvedDocument, kind string) ([]any, error) {
	if len(document.manifest.Documents) == 0 {
		return nil, nil
	}
	parts := make([]any, 0)
	images := 0
	for _, artifact := range document.manifest.Documents[0].Parts {
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
			label := fmt.Sprintf("<document filename=\"%s\" %s>\n%s\n</document>", document.filename, location, text)
			textType := "input_text"
			if kind == "chat" {
				textType = "text"
			}
			parts = append(parts, map[string]any{"type": textType, "text": label})
		}
		if artifact.ImagePath == nil || images >= server.settings.MaxDocumentImages {
			continue
		}
		image, err := os.ReadFile(filepath.Join(document.derivedDir, *artifact.ImagePath))
		if err != nil {
			slog.Error("document image artifact read failed", "filename", document.filename, "part", artifact.PartNumber, "error", err)
			return nil, err
		}
		mediaType := "image/png"
		if artifact.MediaType != nil {
			mediaType = *artifact.MediaType
		}
		dataURL := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(image)
		if kind == "responses" {
			parts = append(parts, map[string]any{"type": "input_image", "detail": "auto", "image_url": dataURL})
		} else {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL}})
		}
		images++
	}
	return parts, nil
}
