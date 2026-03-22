package confluence

import (
	"strings"

	"golang.org/x/net/html"
)

// StripHTML removes HTML tags and returns plain text.
// Confluence storage format is XHTML — this extracts readable text content.
func StripHTML(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return s
	}
	var sb strings.Builder
	extractText(doc, &sb)
	return strings.TrimSpace(sb.String())
}

func extractText(n *html.Node, sb *strings.Builder) {
	if n.Type == html.TextNode {
		sb.WriteString(n.Data)
	}
	if n.Type == html.ElementNode {
		switch n.Data {
		case "br", "p", "div", "li", "h1", "h2", "h3", "h4", "h5", "h6", "tr":
			sb.WriteString("\n")
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		extractText(c, sb)
	}
}
