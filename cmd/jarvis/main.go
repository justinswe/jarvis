package main

import (
	"context"
	"os"

	"github.com/justinswe/std/app"
	"go.uber.org/zap"
)

var rootCmd = newRootCommand()

func main() {
	if err := app.RunCobraCommand(context.Background(), rootCmd); err != nil {
		app.L().Error("Jarvis combined deployment stopped", zap.Error(err))
		os.Exit(1)
	}
}
