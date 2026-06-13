// Package prompts provides a template-based prompt system using Go's text/template.
// Templates are loaded from config/prompts/*.tmpl at startup and cached for reuse.
package prompts

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// Store holds parsed prompt templates keyed by name (filename without extension).
type Store struct {
	templates map[string]*template.Template
	dir       string
}

// NewStore loads all .tmpl files from dir and returns a ready-to-use Store.
func NewStore(dir string) (*Store, error) {
	s := &Store{
		templates: make(map[string]*template.Template),
		dir:       dir,
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading prompts dir %s: %w", dir, err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmpl") {
			continue
		}

		name := strings.TrimSuffix(e.Name(), ".tmpl")
		path := filepath.Join(dir, e.Name())

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading template %s: %w", path, err)
		}

		t, err := template.New(name).Parse(string(data))
		if err != nil {
			return nil, fmt.Errorf("parsing template %s: %w", name, err)
		}

		s.templates[name] = t
	}

	return s, nil
}

// Render executes the named template with data and returns the rendered string.
func (s *Store) Render(name string, data any) (string, error) {
	t, ok := s.templates[name]
	if !ok {
		return "", fmt.Errorf("template %q not found", name)
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("executing template %s: %w", name, err)
	}

	return buf.String(), nil
}

// MustRender is like Render but panics on error. Use only for templates known
// to exist with valid data.
func (s *Store) MustRender(name string, data any) string {
	out, err := s.Render(name, data)
	if err != nil {
		panic(err)
	}
	return out
}

// RenderRaw returns the raw template content without variable substitution.
// Useful for static templates (no placeholders).
func (s *Store) RenderRaw(name string) (string, error) {
	return s.Render(name, nil)
}

// Names returns all loaded template names.
func (s *Store) Names() []string {
	names := make([]string, 0, len(s.templates))
	for k := range s.templates {
		names = append(names, k)
	}
	return names
}
