package discord

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCalculatorEvaluatesLoanFormula(t *testing.T) {
	result, err := (calculatorTool{}).Execute(context.Background(), map[string]any{
		"expression": `18000*(0.06/12)/(1-pow(1+0.06/12,-60))`,
	})
	require.NoError(t, err)
	assert.Equal(t, "347.990427529712", result.(map[string]string)["result"])
}

func TestCalculatorRejectsNonArithmeticAndDivisionByZero(t *testing.T) {
	for _, expression := range []string{`danger()`, `1/0`, `"text"`} {
		_, err := (calculatorTool{}).Execute(context.Background(), map[string]any{"expression": expression})
		assert.Error(t, err)
	}
}
