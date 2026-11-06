package converter

// Options controls document conversion and identifies its source for logging.
type Options struct {
	MaxPages              int
	MaxTextChars          int
	DisableTextExtraction bool
	FileID                string
	Filename              string
}

// Result is the outcome of a conversion: the manifest plus the artifacts and
// warnings produced.
type Result struct {
	Manifest  Manifest
	Artifacts []ManifestPart
	Warnings  []string
}
