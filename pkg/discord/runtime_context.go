package discord

import (
	"context"
	"strings"
	"time"

	"github.com/justinswe/jarvis/pkg/genai"
	googlegenai "google.golang.org/genai"
)

const runtimeContextToolName = "get_runtime_context"

type runtimeContextTool struct {
	version string
	now     func() time.Time
}

type runtimeContextResponse struct {
	Version     string `json:"version"`
	Timezone    string `json:"timezone"`
	CurrentTime string `json:"current_time"`
	CurrentDate string `json:"current_date"`
	Weekday     string `json:"weekday"`
}

func (p *Processor) runtimeContext() genai.FunctionTool {
	return runtimeContextTool{version: p.version, now: time.Now}
}

func (runtimeContextTool) Name() string { return runtimeContextToolName }

func (runtimeContextTool) Declaration() *googlegenai.FunctionDeclaration {
	return &googlegenai.FunctionDeclaration{
		Name: runtimeContextToolName,
		Description: "Read the application's exact build version and current clock information. " +
			"Use only when asked about the application or build version, when asked for the current time, date, or weekday, " +
			"or when the current date materially affects research. Do not call or mention its output in unrelated answers.",
		Parameters: &googlegenai.Schema{Type: googlegenai.TypeObject, Properties: map[string]*googlegenai.Schema{
			"timezone": {
				Type:        googlegenai.TypeString,
				Description: "Optional IANA timezone such as America/Los_Angeles. Omit it to use UTC.",
			},
		}},
	}
}

func (t runtimeContextTool) Execute(_ context.Context, args map[string]any) (any, error) {
	timezone, err := runtimeTimezone(args)
	if err != nil {
		return nil, err
	}
	now := t.now
	if now == nil {
		now = time.Now
	}
	current := now().In(timezone)
	return runtimeContextResponse{
		Version:     strings.TrimSpace(t.version),
		Timezone:    timezone.String(),
		CurrentTime: current.Format(time.RFC3339),
		CurrentDate: current.Format(time.DateOnly),
		Weekday:     current.Weekday().String(),
	}, nil
}

// Evidence records safe runtime values after a successful execution.
func (runtimeContextTool) Evidence(output any) (genai.Evidence, bool) {
	response, ok := output.(runtimeContextResponse)
	if !ok {
		return genai.Evidence{}, false
	}
	return genai.Evidence{
		Kind: genai.EvidenceKindRuntimeContext,
		Tool: runtimeContextToolName,
		Attributes: map[string]string{
			"version":      response.Version,
			"timezone":     response.Timezone,
			"current_time": response.CurrentTime,
			"current_date": response.CurrentDate,
			"weekday":      response.Weekday,
		},
	}, true
}

func runtimeTimezone(args map[string]any) (*time.Location, error) {
	value, exists := args["timezone"]
	if !exists {
		return time.UTC, nil
	}
	name, ok := value.(string)
	if !ok {
		return nil, genai.NewExecutionError("invalid_timezone", "The requested timezone must be a valid IANA timezone.", nil)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return time.UTC, nil
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, genai.NewExecutionError("invalid_timezone", "The requested timezone must be a valid IANA timezone.", err)
	}
	return location, nil
}
