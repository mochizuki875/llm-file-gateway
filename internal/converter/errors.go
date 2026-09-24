package converter

import (
	"errors"
	"fmt"

	"github.com/mochizuki875/llm-file-gateway/internal/converter/extractor"
)

// PageLimitError is returned when a document has more pages than allowed.
type PageLimitError struct {
	Limit int
}

func (err *PageLimitError) Error() string {
	return fmt.Sprintf("document exceeds the %d-page limit", err.Limit)
}

// TextLimitError is returned when extracted text exceeds the character limit.
type TextLimitError struct {
	Limit int
}

func (err *TextLimitError) Error() string {
	return fmt.Sprintf("document exceeds the %d-character text limit", err.Limit)
}

// IsRetryable reports whether a conversion error may succeed when retried
// with the same input. Deterministic errors such as page/text limit
// violations and input validation failures are not retryable.
func IsRetryable(err error) bool {
	var pageLimitError *PageLimitError
	if errors.As(err, &pageLimitError) {
		return false
	}
	var textLimitError *TextLimitError
	if errors.As(err, &textLimitError) {
		return false
	}
	var validationError *extractor.ValidationError
	if errors.As(err, &validationError) {
		return false
	}
	return true
}
