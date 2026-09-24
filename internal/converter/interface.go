package converter

import "context"

// DocumentConverter converts a single file format into extracted text and
// image artifacts.
type DocumentConverter interface {
	Extension() string
	MediaType() string
	Validate(source string) error
	Convert(ctx context.Context, source, outputDir string, options Options) (Result, error)
}
