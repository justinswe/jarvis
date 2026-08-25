package discord

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"strconv"
	"strings"

	"github.com/justinswe/jarvis/worker/pkg/genai"
	"github.com/justinswe/jarvis/worker/pkg/llm"
)

const calculatorToolName = "calculate"

type calculatorTool struct{}

func (calculatorTool) Name() string { return calculatorToolName }

func (calculatorTool) Declaration() *llm.ToolDefinition {
	return &llm.ToolDefinition{
		Name: calculatorToolName,
		Description: "Evaluate exact arithmetic. Use for calculations instead of mental math. " +
			"Supports +, -, *, /, parentheses, and pow(base, exponent).",
		InputSchema: objectSchema(map[string]any{
			"expression": stringSchema("Arithmetic expression using numeric literals, +, -, *, /, parentheses, and pow(base, exponent)."),
		}, []string{"expression"}),
		Effect: llm.ToolEffectReadOnly,
	}
}

func (calculatorTool) Execute(_ context.Context, args map[string]any) (any, error) {
	expression, _ := args["expression"].(string)
	expression = strings.TrimSpace(expression)
	if expression == "" || len(expression) > 256 {
		return nil, genai.NewExecutionError("invalid_calculation", "expression must contain at most 256 characters", nil)
	}
	parsed, err := parser.ParseExpr(expression)
	if err != nil {
		return nil, genai.NewExecutionError("invalid_calculation", "expression is not valid arithmetic", err)
	}
	result, err := evaluateArithmetic(parsed, 0)
	if err != nil || math.IsInf(result, 0) || math.IsNaN(result) {
		return nil, genai.NewExecutionError("invalid_calculation", "expression could not be evaluated to a finite number", err)
	}
	return map[string]string{
		"expression": expression,
		"result":     strconv.FormatFloat(result, 'g', 15, 64),
	}, nil
}

func evaluateArithmetic(expression ast.Expr, depth int) (float64, error) {
	if depth > 32 {
		return 0, genai.NewExecutionError("invalid_calculation", "expression is too deeply nested", nil)
	}
	switch value := expression.(type) {
	case *ast.BasicLit:
		if value.Kind != token.INT && value.Kind != token.FLOAT {
			return 0, genai.NewExecutionError("invalid_calculation", "only numeric literals are supported", nil)
		}
		return strconv.ParseFloat(value.Value, 64)
	case *ast.ParenExpr:
		return evaluateArithmetic(value.X, depth+1)
	case *ast.UnaryExpr:
		operand, err := evaluateArithmetic(value.X, depth+1)
		if err != nil {
			return 0, err
		}
		switch value.Op {
		case token.ADD:
			return operand, nil
		case token.SUB:
			return -operand, nil
		default:
			return 0, genai.NewExecutionError("invalid_calculation", "unsupported unary operator", nil)
		}
	case *ast.BinaryExpr:
		left, err := evaluateArithmetic(value.X, depth+1)
		if err != nil {
			return 0, err
		}
		right, err := evaluateArithmetic(value.Y, depth+1)
		if err != nil {
			return 0, err
		}
		switch value.Op {
		case token.ADD:
			return left + right, nil
		case token.SUB:
			return left - right, nil
		case token.MUL:
			return left * right, nil
		case token.QUO:
			if right == 0 {
				return 0, genai.NewExecutionError("invalid_calculation", "division by zero is undefined", nil)
			}
			return left / right, nil
		default:
			return 0, genai.NewExecutionError("invalid_calculation", "unsupported binary operator", nil)
		}
	case *ast.CallExpr:
		name, ok := value.Fun.(*ast.Ident)
		if !ok || name.Name != "pow" || len(value.Args) != 2 {
			return 0, genai.NewExecutionError("invalid_calculation", "only pow(base, exponent) calls are supported", nil)
		}
		base, err := evaluateArithmetic(value.Args[0], depth+1)
		if err != nil {
			return 0, err
		}
		exponent, err := evaluateArithmetic(value.Args[1], depth+1)
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exponent), nil
	default:
		return 0, genai.NewExecutionError("invalid_calculation", "unsupported expression", nil)
	}
}

func calculationRelevant(request string) bool {
	lower := strings.ToLower(request)
	for _, word := range []string{"calculate", "calculation", "payment", "loan", "mortgage", "interest", "percent", "percentage", "total", "arithmetic"} {
		if strings.Contains(lower, word) {
			return true
		}
	}
	return strings.ContainsAny(lower, "0123456789") && strings.ContainsAny(lower, "+-*/")
}
