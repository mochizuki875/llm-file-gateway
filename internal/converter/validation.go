package converter

import (
	"errors"
	"io"
	"os"
)

// validateSignature checks that the file at source begins with one of the
// given magic byte signatures.
func validateSignature(source string, signatures ...[]byte) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	header := make([]byte, 8)
	read, err := io.ReadFull(input, header)
	if err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	header = header[:read]
	for _, signature := range signatures {
		if len(header) >= len(signature) && string(header[:len(signature)]) == string(signature) {
			return nil
		}
	}
	return errors.New("file content does not match its extension")
}
