package converter

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

type textExtractor func(string) (string, error)

type textConverter struct {
	extension string
	mediaType string
	extract   textExtractor
}

func newTextConverter(extension, mediaType string, extract textExtractor) textConverter {
	return textConverter{extension: extension, mediaType: mediaType, extract: extract}
}

func (documentConverter textConverter) Extension() string { return documentConverter.extension }
func (documentConverter textConverter) MediaType() string { return documentConverter.mediaType }
func (documentConverter textConverter) Validate(source string) error {
	return validateExtractedText(source, documentConverter.extract)
}
func (documentConverter textConverter) Convert(_ context.Context, source, outputDir string, options Options) (Result, error) {
	return convertExtractedText(source, outputDir, documentConverter.mediaType, documentConverter.extract, options)
}

func validateExtractedText(source string, extract textExtractor) error {
	_, err := extract(source)
	return err
}

func convertExtractedText(source, outputDir, mediaType string, extract textExtractor, options Options) (Result, error) {
	text, err := extract(source)
	if err != nil {
		return Result{}, err
	}
	return convertTextDocument(source, outputDir, mediaType, []string{text}, options)
}

func extractPlainText(source string) (string, error) {
	return readUTF8(source)
}

func extractCSVText(source string) (string, error) {
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
			return "", fmt.Errorf("CSV file is malformed: %w", err)
		}
		lines = append(lines, strings.Join(row, "\t"))
	}
	return strings.Join(lines, "\n"), nil
}

func extractHTMLText(source string) (string, error) {
	text, err := readUTF8(source)
	if err != nil {
		return "", err
	}
	return visibleHTMLText(text)
}

func visibleHTMLText(source string) (string, error) {
	root, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "", err
	}
	hidden := map[string]bool{"head": true, "script": true, "style": true, "template": true}
	block := map[string]bool{
		"address": true, "article": true, "aside": true, "blockquote": true, "br": true,
		"div": true, "footer": true, "h1": true, "h2": true, "h3": true, "h4": true,
		"h5": true, "h6": true, "header": true, "hr": true, "li": true, "main": true,
		"nav": true, "ol": true, "p": true, "pre": true, "section": true, "table": true,
		"td": true, "th": true, "tr": true, "ul": true,
	}
	var output strings.Builder
	var walk func(*html.Node, bool)
	walk = func(node *html.Node, insideHidden bool) {
		name := strings.ToLower(node.Data)
		insideHidden = insideHidden || (node.Type == html.ElementNode && hidden[name])
		if !insideHidden && node.Type == html.ElementNode && block[name] {
			output.WriteByte('\n')
		}
		if !insideHidden && node.Type == html.TextNode {
			output.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child, insideHidden)
		}
		if !insideHidden && node.Type == html.ElementNode && block[name] {
			output.WriteByte('\n')
		}
	}
	walk(root, false)
	var lines []string
	for _, line := range strings.Split(output.String(), "\n") {
		if normalized := strings.Join(strings.Fields(line), " "); normalized != "" {
			lines = append(lines, normalized)
		}
	}
	return strings.Join(lines, "\n"), nil
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
