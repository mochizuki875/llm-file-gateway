package converter

// Options controls the limits applied during document conversion.
type Options struct {
	MaxPages              int
	MaxTextChars          int
	DisableTextExtraction bool
}

// Artifact describes a single converted part (text and/or image) of a document.
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
	Name     string     `json:"name"`
	TextPath string     `json:"text_path,omitempty"`
	Parts    []Artifact `json:"parts"`
}

// Result is the outcome of a conversion: the manifest plus the artifacts and
// warnings produced.
type Result struct {
	Manifest  Manifest
	Artifacts []Artifact
	Warnings  []string
}
