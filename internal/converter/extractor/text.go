package extractor

import (
	"errors"
	"os"
	"strings"
	"unicode/utf8"
)

// PlainText reads a UTF-8 text file and returns its content as-is.
func PlainText(source string) (string, error) {
	return readUTF8(source)
}

func readUTF8(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(content) >= 3 && string(content[:3]) == "\xef\xbb\xbf" {
		content = content[3:]
	}
	if !utf8.Valid(content) {
		return "", errors.New("text file must be valid UTF-8")
	}
	if strings.IndexByte(string(content), 0) >= 0 {
		return "", errors.New("text file must not contain null bytes")
	}
	return string(content), nil
}
