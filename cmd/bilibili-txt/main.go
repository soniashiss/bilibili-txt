package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"bilibili-txt/internal/pipeline"
)

var version = "0.0.0-dev"

func main() {
	err := newRootCmd().Execute()
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "bilibili-txt: %s\n", err)
	os.Exit(classifyExit(err))
}

func classifyExit(err error) int {
	switch {
	case errors.Is(err, context.Canceled):
		return 130
	case errors.Is(err, pipeline.ErrConflictUnresolved):
		return 3
	case errors.Is(err, errPreflightFailed):
		return 2
	default:
		return 1
	}
}
