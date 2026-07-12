package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stderr bytes.Buffer
	if got := Run(context.Background(), []string{"unknown"}, &bytes.Buffer{}, &stderr); got != 2 {
		t.Fatalf("exit code = %d", got)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunRecognizesCommands(t *testing.T) {
	for _, command := range []string{"serve", "import", "upload", "version", "help"} {
		t.Run(command, func(t *testing.T) {
			if got := Run(context.Background(), []string{command}, &bytes.Buffer{}, &bytes.Buffer{}); got != 0 {
				t.Fatalf("exit code = %d", got)
			}
		})
	}
}
