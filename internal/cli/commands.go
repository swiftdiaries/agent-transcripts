package cli

import (
	"context"
	"fmt"
	"io"
)

const Version = "dev"

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	_ = ctx
	if len(args) == 0 {
		return usage(stdout)
	}
	switch args[0] {
	case "help":
		return usage(stdout)
	case "version":
		_, _ = fmt.Fprintln(stdout, Version)
		return 0
	case "serve", "import", "upload":
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func usage(w io.Writer) int {
	_, _ = fmt.Fprintln(w, "usage: agent-transcripts <serve|import|upload|version|help>")
	return 0
}
