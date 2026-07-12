package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
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
	case "import":
		return runImport(ctx, args[1:], stdout, stderr)
	case "serve", "upload":
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

type importOptions struct {
	path     string
	latest   bool
	provider string
	limit    int
}

func parseImportArgs(args []string) (importOptions, error) {
	var got importOptions
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&got.latest, "latest", false, "select the newest eligible session")
	flags.StringVar(&got.provider, "provider", "", "filter by claude or codex")
	flags.IntVar(&got.limit, "limit", 20, "maximum choices")
	if err := flags.Parse(args); err != nil {
		return got, err
	}
	if got.provider != "" && got.provider != "claude" && got.provider != "codex" {
		return got, errors.New("provider must be claude or codex")
	}
	if got.limit < 1 {
		return got, errors.New("limit must be positive")
	}
	if flags.NArg() > 1 {
		return got, errors.New("import accepts at most one path")
	}
	if flags.NArg() == 1 {
		got.path = flags.Arg(0)
	}
	if got.latest && got.path != "" {
		return got, errors.New("--latest and a path are mutually exclusive")
	}
	return got, nil
}

func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseImportArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if opts.path != "" {
		candidate, err := discovery.InspectPath(ctx, opts.path, time.Now(), 5*time.Minute)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if opts.provider != "" && candidate.Provider != opts.provider {
			_, _ = fmt.Fprintln(stderr, "source does not match --provider")
			return 1
		}
		return emitEligible(candidate, stdout, stderr)
	}
	interactive := false
	if file, ok := stdout.(*os.File); ok {
		if info, statErr := file.Stat(); statErr == nil {
			interactive = info.Mode()&os.ModeCharDevice != 0
		}
	}
	if !opts.latest && !interactive {
		_, _ = fmt.Fprintln(stderr, "non-interactive import requires a path or --latest")
		return 2
	}
	candidates, err := discovery.Discover(ctx, defaultRoots(), time.Now(), 5*time.Minute)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	candidates = filterCandidates(candidates, opts)
	if len(candidates) == 0 {
		_, _ = fmt.Fprintln(stderr, "no eligible completed sessions")
		return 1
	}
	if opts.latest {
		return emitEligible(candidates[0], stdout, stderr)
	}
	for i, candidate := range candidates {
		_, _ = fmt.Fprintf(stdout, "%d) %s  %s  %s\n", i+1, candidate.Provider, candidate.Project, candidate.Title)
	}
	_, _ = fmt.Fprint(stdout, "Select sessions (comma-separated): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	indexes, err := parseSelections(line, len(candidates))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	for _, index := range indexes {
		if code := emitEligible(candidates[index], stdout, stderr); code != 0 {
			return code
		}
	}
	return 0
}

func defaultRoots() discovery.Roots {
	home, _ := os.UserHomeDir()
	return discovery.Roots{Claude: []string{filepath.Join(home, ".claude", "projects")}, Codex: []string{filepath.Join(home, ".codex", "sessions"), filepath.Join(home, ".codex", "archived_sessions")}}
}

func filterCandidates(candidates []discovery.Candidate, opts importOptions) []discovery.Candidate {
	filtered := make([]discovery.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		if opts.provider == "" || candidate.Provider == opts.provider {
			filtered = append(filtered, candidate)
		}
		if len(filtered) == opts.limit {
			break
		}
	}
	if opts.latest && len(filtered) > 1 {
		filtered = filtered[:1]
	}
	return filtered
}

func parseSelections(value string, maximum int) ([]int, error) {
	var got []int
	seen := map[int]bool{}
	for _, field := range strings.Split(strings.TrimSpace(value), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 1 || n > maximum {
			return nil, errors.New("selection must contain valid comma-separated numbers")
		}
		if !seen[n] {
			got = append(got, n-1)
			seen[n] = true
		}
	}
	if len(got) == 0 {
		return nil, errors.New("no sessions selected")
	}
	return got, nil
}

func emitEligible(candidate discovery.Candidate, stdout, stderr io.Writer) int {
	reader, _, err := discovery.OpenEligible(candidate)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_ = reader.Close()
	_, _ = fmt.Fprintln(stdout, candidate.Path)
	return 0
}

func usage(w io.Writer) int {
	_, _ = fmt.Fprintln(w, "usage: agent-transcripts <serve|import|upload|version|help>")
	return 0
}
