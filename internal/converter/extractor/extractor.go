package extractor

// Extractor reads a source file and returns its extracted text.
type Extractor func(string) (string, error)
