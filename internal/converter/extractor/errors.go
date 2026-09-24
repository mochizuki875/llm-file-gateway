package extractor

// ValidationError is returned when a source file's content prevents
// extraction, such as invalid UTF-8, null bytes, or malformed CSV. These
// errors are deterministic and will not succeed when retried with the same
// input.
type ValidationError struct {
	Message string
}

func (err *ValidationError) Error() string {
	return err.Message
}
