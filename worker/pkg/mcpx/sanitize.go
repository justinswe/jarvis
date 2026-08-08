package mcpx

import (
	"github.com/justinswe/jarvis/worker/pkg/llm"
)

// SanitizeSchema downgrades an MCP tool's inputSchema to the strict subset the llm
// package validates and every configured provider accepts. Real MCP servers routinely
// emit anyOf/oneOf/$ref and typeless properties; those degrade to a string property
// documented as JSON-encoded rather than rejecting the whole tool. A tool whose root
// is not an object schema is unrepresentable and reported false.
func SanitizeSchema(raw any) (llm.JSONSchema, bool) {
	object, ok := raw.(map[string]any)
	if !ok {
		return nil, false
	}
	if kind, _ := object["type"].(string); kind != "object" {
		return nil, false
	}
	return llm.JSONSchema(sanitizeObject(object)), true
}

func sanitizeObject(object map[string]any) map[string]any {
	result := map[string]any{"type": "object"}
	if description, ok := object["description"].(string); ok && description != "" {
		result["description"] = description
	}
	properties := map[string]any{}
	if rawProperties, ok := object["properties"].(map[string]any); ok {
		for name, value := range rawProperties {
			properties[name] = sanitizeProperty(value)
		}
	}
	result["properties"] = properties
	if rawRequired, ok := object["required"].([]any); ok {
		var kept []any
		for _, entry := range rawRequired {
			if name, ok := entry.(string); ok && name != "" {
				if _, declared := properties[name]; declared {
					kept = append(kept, name)
				}
			}
		}
		if len(kept) > 0 {
			result["required"] = kept
		}
	}
	return result
}

func sanitizeProperty(value any) map[string]any {
	property, ok := value.(map[string]any)
	if !ok {
		return map[string]any{"type": "string"}
	}
	kind, _ := property["type"].(string)
	switch kind {
	case "string", "number", "integer", "boolean", "null":
		result := map[string]any{"type": kind}
		if description, ok := property["description"].(string); ok && description != "" {
			result["description"] = description
		}
		if enum, ok := property["enum"].([]any); ok && len(enum) > 0 {
			result["enum"] = enum
		}
		return result
	case "object":
		return sanitizeObject(property)
	case "array":
		result := map[string]any{"type": "array", "items": sanitizeProperty(property["items"])}
		if description, ok := property["description"].(string); ok && description != "" {
			result["description"] = description
		}
		return result
	default:
		// anyOf/oneOf/$ref/typeless: accept any value as a JSON-encoded string.
		description, _ := property["description"].(string)
		if description != "" {
			description += " "
		}
		return map[string]any{"type": "string", "description": description + "(JSON-encoded value)"}
	}
}
