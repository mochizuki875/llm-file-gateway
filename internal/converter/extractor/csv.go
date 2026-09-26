package extractor

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// ExtractCSV parses a CSV file and joins each record with tabs.
func ExtractCSV(source string) (string, error) {
	text, err := readUTF8(source)
	if err != nil {
		return "", err
	}
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	var lines []string
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", &ValidationError{Message: fmt.Sprintf("CSV file is malformed: %v", err)}
		}
		lines = append(lines, strings.Join(row, "\t"))
	}
	return strings.Join(lines, "\n"), nil
}
