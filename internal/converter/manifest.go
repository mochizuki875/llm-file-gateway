package converter

import "path/filepath"

const (
	// ManifestSchemaVersion is the schema version written to manifest.json.
	ManifestSchemaVersion = 1
	// ConverterVersion identifies the converter implementation that produced a manifest.
	ConverterVersion = "2026.09.28"
)

// ManifestPart describes a single converted part (text and/or image) of a document.
type ManifestPart struct {
	PartNumber int     `json:"part_number"`
	PageNumber *int    `json:"page_number"`
	TextPath   string  `json:"text_path"`
	ImagePath  *string `json:"image_path"`
	Width      *int    `json:"width"`
	Height     *int    `json:"height"`
	MediaType  *string `json:"media_type"`
	SHA256     *string `json:"sha256"`
}

// Manifest describes the conversion output of a document: the source metadata
// and the list of documents with their text and image artifacts.
type Manifest struct {
	SchemaVersion    int                `json:"schema_version"`
	ConverterVersion string             `json:"converter_version"`
	Source           ManifestSource     `json:"source"`
	Documents        []ManifestDocument `json:"documents"`
	Warnings         []string           `json:"warnings"`
}

// ManifestSource identifies the original source file.
type ManifestSource struct {
	MediaType string `json:"media_type"`
	SHA256    string `json:"sha256"`
}

// ManifestDocument is one converted document (usually a single file).
type ManifestDocument struct {
	Name  string         `json:"name"`
	Parts []ManifestPart `json:"parts"`
}

func newManifest(source, mediaType, digest string, artifacts []ManifestPart) Manifest {
	return Manifest{
		SchemaVersion:    ManifestSchemaVersion,
		ConverterVersion: ConverterVersion,
		Source:           ManifestSource{MediaType: mediaType, SHA256: digest},
		Documents:        []ManifestDocument{{Name: filepath.Base(source), Parts: artifacts}},
		Warnings:         []string{},
	}
}
