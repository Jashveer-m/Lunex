package tools

// Schema is the subset of JSON Schema the registry needs to describe a tool's
// input and output. It marshals to standard JSON Schema, so a later provider
// with native tool calling -- or a client listing what the assistant can do --
// can be handed it unchanged.
type Schema struct {
	Type        string            `json:"type"`
	Description string            `json:"description,omitempty"`
	Format      string            `json:"format,omitempty"`
	Enum        []string          `json:"enum,omitempty"`
	Items       *Schema           `json:"items,omitempty"`
	Properties  map[string]Schema `json:"properties,omitempty"`
	Required    []string          `json:"required,omitempty"`
}

// InputSchema is the tool's input as JSON Schema, generated from its Params so
// the two cannot disagree.
func (t Tool) InputSchema() Schema {
	s := Schema{Type: "object", Properties: make(map[string]Schema, len(t.Params))}
	for _, p := range t.Params {
		prop := Schema{Type: p.Type, Description: p.Description, Enum: p.Enum}
		if p.Type == "array" {
			prop.Items = &Schema{Type: "string"}
		}
		s.Properties[p.Name] = prop
		if p.Required {
			s.Required = append(s.Required, p.Name)
		}
	}
	return s
}

// Small constructors for the output schemas below, which would otherwise be a
// page of nested literals per tool.

func object(props map[string]Schema, required ...string) Schema {
	return Schema{Type: "object", Properties: props, Required: required}
}

func str(description string) Schema { return Schema{Type: "string", Description: description} }

func uuidField(description string) Schema {
	return Schema{Type: "string", Format: "uuid", Description: description}
}

func timestamp(description string) Schema {
	return Schema{Type: "string", Format: "date-time", Description: description}
}

func listOf(item Schema) Schema { return Schema{Type: "array", Items: &item} }

func integer(description string) Schema { return Schema{Type: "integer", Description: description} }

func boolean(description string) Schema { return Schema{Type: "boolean", Description: description} }

func number(description string) Schema { return Schema{Type: "number", Description: description} }
