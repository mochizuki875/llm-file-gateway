package extractor

import (
	"strings"

	"golang.org/x/net/html"
)

// HTML parses an HTML file and returns its visible text.
func HTML(source string) (string, error) {
	text, err := readUTF8(source)
	if err != nil {
		return "", err
	}
	return visibleHTMLText(text)
}

// visibleHTMLText walks the parsed HTML tree and returns the visible text,
// skipping hidden elements (head, script, style, template) and normalizing
// whitespace.
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
