package apierror

import "fmt"

// Error is an OpenAI-format API error with an HTTP status, error code, and
// optional parameter name.
type Error struct {
	Status  int
	Code    string
	Message string
	Param   string
}

func (err *Error) Error() string { return err.Message }

// New creates an API error with the given status, code, message, and param.
func New(status int, code, message, param string) *Error {
	return &Error{Status: status, Code: code, Message: message, Param: param}
}

// FileTooLarge returns a 400 error describing the configured file size limit.
func FileTooLarge(maxBytes int64, param string) *Error {
	display := fmt.Sprintf("%d bytes", maxBytes)
	if maxBytes >= 1024*1024 {
		display += fmt.Sprintf(" (%g MiB)", float64(maxBytes)/(1024*1024))
	} else if maxBytes >= 1024 {
		display += fmt.Sprintf(" (%g KiB)", float64(maxBytes)/1024)
	}
	return New(400, "file_too_large", "File exceeds the configured limit of "+display+".", param)
}
