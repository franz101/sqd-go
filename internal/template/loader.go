package template

import (
	"embed"
	"strings"
	"text/template"
)

//go:embed templates/sql/*.tmpl templates/code/*.tmpl
var templateFS embed.FS

// Template caches parsed templates for reuse
var cachedTemplates map[string]*template.Template

func init() {
	cachedTemplates = make(map[string]*template.Template)

	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}

	// Pre-load and parse all templates
	sqlFiles, _ := templateFS.ReadDir("templates/sql")
	for _, f := range sqlFiles {
		if !strings.HasSuffix(f.Name(), ".tmpl") {
			continue
		}
		content, _ := templateFS.ReadFile("templates/sql/" + f.Name())
		tmpl, err := template.New(f.Name()).Funcs(funcMap).Parse(string(content))
		if err != nil {
			panic("failed to parse template sql/" + f.Name() + ": " + err.Error())
		}

		// Cache each named template within the file
		for _, name := range tmpl.Templates() {
			if name.Name() != "" {
				cachedTemplates["sql/"+name.Name()] = tmpl.Lookup(name.Name())
			}
		}
	}

	codeFiles, _ := templateFS.ReadDir("templates/code")
	for _, f := range codeFiles {
		if !strings.HasSuffix(f.Name(), ".tmpl") {
			continue
		}
		content, _ := templateFS.ReadFile("templates/code/" + f.Name())
		tmpl, err := template.New(f.Name()).Funcs(funcMap).Parse(string(content))
		if err != nil {
			panic("failed to parse template code/" + f.Name() + ": " + err.Error())
		}

		// Cache each named template within the file
		for _, name := range tmpl.Templates() {
			if name.Name() != "" {
				cachedTemplates["code/"+name.Name()] = tmpl.Lookup(name.Name())
			}
		}
	}
}

// MustExecute executes a cached template by name, panicking on error.
// The name should be in the format "category/templateName" (e.g., "sql/createBlocksTable").
func MustExecute(name string, data interface{}) string {
	tmpl, ok := cachedTemplates[name]
	if !ok {
		panic("template not found: " + name)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		panic("template execution failed for " + name + ": " + err.Error())
	}
	return buf.String()
}

// Execute executes a cached template by name, returning an error on failure.
func Execute(name string, data interface{}) (string, error) {
	tmpl, ok := cachedTemplates[name]
	if !ok {
		return "", &NotFoundError{Name: name}
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// List returns all available template names.
func List() []string {
	names := make([]string, 0, len(cachedTemplates))
	for name := range cachedTemplates {
		names = append(names, name)
	}
	return names
}

// NotFoundError is returned when a requested template doesn't exist.
type NotFoundError struct {
	Name string
}

func (e *NotFoundError) Error() string {
	return "template not found: " + e.Name
}

// IsNotFound checks if an error is a NotFoundError.
func IsNotFound(err error) bool {
	_, ok := err.(*NotFoundError)
	return ok
}
