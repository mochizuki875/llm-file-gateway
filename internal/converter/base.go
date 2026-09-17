package converter

import (
	"context"
	"fmt"
)

type Options struct {
	MaxPages              int
	MaxTextChars          int
	DisableTextExtraction bool
}

type Artifact struct {
	PartNumber int     `json:"part_number"`
	PageNumber *int    `json:"page_number"`
	TextPath   string  `json:"text_path"`
	ImagePath  *string `json:"image_path"`
	Width      *int    `json:"width"`
	Height     *int    `json:"height"`
	MediaType  *string `json:"media_type"`
	SHA256     *string `json:"sha256"`
}

type Manifest struct {
	SchemaVersion    int                `json:"schema_version"`
	ConverterVersion string             `json:"converter_version"`
	Source           ManifestSource     `json:"source"`
	Documents        []ManifestDocument `json:"documents"`
	Warnings         []string           `json:"warnings"`
}

type ManifestSource struct {
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
}

type ManifestDocument struct {
	Name  string     `json:"name"`
	Parts []Artifact `json:"parts"`
}

type Result struct {
	Manifest  Manifest
	Artifacts []Artifact
	Warnings  []string
}

type DocumentConverter interface {
	Extension() string
	MediaType() string
	Validate(source string) error
	Convert(ctx context.Context, source, outputDir string, options Options) (Result, error)
}

type PageLimitError struct {
	Limit int
}

func (err *PageLimitError) Error() string {
	return fmt.Sprintf("document exceeds the %d-page limit", err.Limit)
}

type TextLimitError struct {
	Limit int
}

func (err *TextLimitError) Error() string {
	return fmt.Sprintf("document exceeds the %d-character text limit", err.Limit)
}
