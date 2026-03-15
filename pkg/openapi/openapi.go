package openapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/invopop/jsonschema"
)

// SpecConfig holds the metadata for the OpenAPI spec.
type SpecConfig struct {
	Title       string
	Description string
	Version     string
}

// SchemaFromType uses invopop/jsonschema to reflect a Go struct into an
// openapi3.SchemaRef. It reads json tags for field names and
// jsonschema/jsonschema_description tags for descriptions and validation.
func SchemaFromType(v any) *openapi3.SchemaRef {
	r := &jsonschema.Reflector{DoNotReference: true}
	js := r.Reflect(v)

	data, err := json.Marshal(js)
	if err != nil {
		return &openapi3.SchemaRef{Value: openapi3.NewObjectSchema()}
	}

	var s openapi3.Schema
	if err := json.Unmarshal(data, &s); err != nil {
		return &openapi3.SchemaRef{Value: openapi3.NewObjectSchema()}
	}

	return &openapi3.SchemaRef{Value: &s}
}

// Ref builds a $ref to #/components/schemas/<name>.
func Ref(name string) *openapi3.SchemaRef {
	return openapi3.NewSchemaRef("#/components/schemas/"+name, nil)
}

// ArrayOf builds an array schema whose items $ref a named component schema.
func ArrayOf(name string) *openapi3.SchemaRef {
	s := openapi3.NewArraySchema()
	s.Items = Ref(name)
	return &openapi3.SchemaRef{Value: s}
}

// NewOp builds an Operation with optional request body and a single response.
func NewOp(id, tag, desc string, body *openapi3.SchemaRef, status int, resp *openapi3.SchemaRef) *openapi3.Operation {
	op := openapi3.NewOperation()
	op.OperationID = id
	op.Tags = []string{tag}
	op.Description = desc
	if body != nil {
		op.RequestBody = &openapi3.RequestBodyRef{
			Value: openapi3.NewRequestBody().WithRequired(true).WithJSONSchemaRef(body),
		}
	}
	op.AddResponse(status, openapi3.NewResponse().
		WithDescription(desc).
		WithJSONSchemaRef(resp))
	return op
}

// NewOpWithPathParam is like NewOp but adds a required path parameter.
func NewOpWithPathParam(id, tag, desc, param string, body *openapi3.SchemaRef, status int, resp *openapi3.SchemaRef) *openapi3.Operation {
	op := NewOp(id, tag, desc, body, status, resp)
	op.Parameters = openapi3.Parameters{
		&openapi3.ParameterRef{
			Value: openapi3.NewPathParameter(param).
				WithSchema(&openapi3.Schema{Type: &openapi3.Types{"string"}}),
		},
	}
	return op
}

// BuildSchemas generates openapi3.Schemas from a name->Go type registry.
func BuildSchemas(registry map[string]any) openapi3.Schemas {
	schemas := make(openapi3.Schemas, len(registry))
	for name, typ := range registry {
		schemas[name] = SchemaFromType(typ)
	}
	return schemas
}

// NewAPIDocs returns an http.HandlerFunc that serves a Scalar API docs page.
// scalarJS is the bundled Scalar standalone JS, specPath is the URL to the
// OpenAPI spec (e.g. "/v1/openapi.json").
func NewAPIDocs(title string, specPath string, scalarJS []byte) http.HandlerFunc {
	header := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <title>%s</title>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
</head>
<body>
  <script id="api-reference" data-url="%s"></script>
  <script>
`, title, specPath)

	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(header))
		w.Write(scalarJS)
		w.Write([]byte("\n</script>\n</body>\n</html>\n"))
	}
}
