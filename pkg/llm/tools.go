package llm

import (
	"regexp"
	"strings"

	"github.com/justinswe/std/errors"
)

var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// validateTools rejects malformed declarations before they reach a provider.
func validateTools(tools []ToolDefinition) error {
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if !toolNamePattern.MatchString(tool.Name) {
			return errors.Errorf("tool name %q is invalid", tool.Name)
		}
		if _, ok := seen[tool.Name]; ok {
			return errors.Errorf("tool name %q is duplicated", tool.Name)
		}
		seen[tool.Name] = struct{}{}
		if err := validateObjectSchema(tool.InputSchema); err != nil {
			return errors.Wrapf(err, "tool %q input schema", tool.Name)
		}
	}
	return nil
}

func normalizeToolChoice(choice ToolChoice, tools []ToolDefinition) (ToolChoice, error) {
	if choice.Mode == "" {
		return ToolChoice{}, nil
	}
	switch choice.Mode {
	case ToolChoiceDisabled:
		if strings.TrimSpace(choice.FunctionName) != "" {
			return ToolChoice{}, errors.New("disabled tool choice must not name a function")
		}
		return ToolChoice{Mode: ToolChoiceDisabled}, nil
	case ToolChoiceAutomatic, ToolChoiceRequired:
		if len(tools) == 0 {
			return ToolChoice{}, errors.New("tool choice requires at least one tool")
		}
		if strings.TrimSpace(choice.FunctionName) != "" {
			return ToolChoice{}, errors.New("tool choice mode must not name a function")
		}
		return ToolChoice{Mode: choice.Mode}, nil
	case ToolChoiceFunction:
		name := strings.TrimSpace(choice.FunctionName)
		if name == "" {
			return ToolChoice{}, errors.New("specific tool choice requires a function name")
		}
		for _, tool := range tools {
			if tool.Name == name {
				return ToolChoice{Mode: ToolChoiceFunction, FunctionName: name}, nil
			}
		}
		return ToolChoice{}, errors.Errorf("specific tool choice references unknown function %q", name)
	default:
		return ToolChoice{}, errors.Errorf("unsupported tool choice mode %q", choice.Mode)
	}
}

func validateObjectSchema(schema JSONSchema) error {
	if schema == nil {
		return errors.New("schema is required")
	}
	schemaType, ok := schema["type"].(string)
	if !ok || !strings.EqualFold(schemaType, "object") {
		return errors.New("root type must be object")
	}
	properties, err := schemaProperties(schema["properties"])
	if err != nil {
		return err
	}
	for name, property := range properties {
		if err := validatePropertySchema(property); err != nil {
			return errors.Wrapf(err, "property %q", name)
		}
	}
	required, err := schemaRequired(schema["required"])
	if err != nil {
		return err
	}
	for _, name := range required {
		if _, ok := properties[name]; !ok {
			return errors.Errorf("required property %q is not declared", name)
		}
	}
	return nil
}

func schemaProperties(value any) (map[string]any, error) {
	if value == nil {
		return map[string]any{}, nil
	}
	properties, ok := value.(map[string]any)
	if ok {
		return properties, nil
	}
	if schema, ok := value.(JSONSchema); ok {
		return map[string]any(schema), nil
	}
	return nil, errors.New("properties must be an object")
}

func validatePropertySchema(value any) error {
	schema, err := schemaMap(value)
	if err != nil {
		return err
	}
	schemaType, ok := schema["type"].(string)
	if !ok || strings.TrimSpace(schemaType) == "" {
		return errors.New("type must be a string")
	}
	switch strings.ToLower(schemaType) {
	case "string", "number", "integer", "boolean", "null":
		return nil
	case "object":
		return validateObjectSchema(JSONSchema(schema))
	case "array":
		if schema["items"] == nil {
			return errors.New("array items schema is required")
		}
		return validatePropertySchema(schema["items"])
	default:
		return errors.Errorf("type %q is unsupported", schemaType)
	}
}

func schemaMap(value any) (map[string]any, error) {
	switch schema := value.(type) {
	case map[string]any:
		return schema, nil
	case JSONSchema:
		return map[string]any(schema), nil
	default:
		return nil, errors.New("schema must be an object")
	}
}

func schemaRequired(value any) ([]string, error) {
	switch required := value.(type) {
	case nil:
		return nil, nil
	case []string:
		for _, name := range required {
			if strings.TrimSpace(name) == "" {
				return nil, errors.New("required entries must be non-empty strings")
			}
		}
		return required, nil
	case []any:
		result := make([]string, 0, len(required))
		for _, item := range required {
			name, ok := item.(string)
			if !ok || strings.TrimSpace(name) == "" {
				return nil, errors.New("required entries must be non-empty strings")
			}
			result = append(result, name)
		}
		return result, nil
	default:
		return nil, errors.New("required must be an array of strings")
	}
}
