package converter

// Options controls the limits applied during document conversion.
type Options struct {
	MaxPages              int
	MaxTextChars          int
	DisableTextExtraction bool
}

// Result is the outcome of a conversion: the manifest plus the artifacts and
// warnings produced.
type Result struct {
	Manifest  Manifest
	Artifacts []ManifestPart
	Warnings  []string
}
