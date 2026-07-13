package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/config"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
	"github.com/swiftdiaries/agent-transcripts/internal/web"
	"golang.org/x/term"
)

const Version = "dev"

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
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
		return runImport(ctx, args[1:], os.Stdin, stdout, stderr)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "upload":
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

type serveOptions struct {
	configPath string
	open       bool
}

func parseServeArgs(args []string) (serveOptions, error) {
	var got serveOptions
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&got.configPath, "config", "", "configuration file")
	flags.BoolVar(&got.open, "open", false, "open the local browser")
	if err := flags.Parse(args); err != nil {
		return got, err
	}
	if flags.NArg() != 0 {
		return got, errors.New("serve accepts no positional arguments")
	}
	return got, nil
}

func runServe(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runServeWithOpener(ctx, args, stdout, stderr, openBrowser)
}

func runServeWithOpener(ctx context.Context, args []string, stdout, stderr io.Writer, opener func(string, io.Writer)) int {
	return runServeWithDeps(ctx, args, stdout, stderr, opener, net.Listen)
}

func runServeWithDeps(ctx context.Context, args []string, stdout, stderr io.Writer, opener func(string, io.Writer), listen func(string, string) (net.Listener, error)) int {
	opts, err := parseServeArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	cfg, err := config.Load(opts.configPath, config.Overrides{})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	if cfg.Mode != "local" {
		_, _ = fmt.Fprintln(stderr, "serve only supports local mode until hosted authentication is configured")
		return 1
	}
	if cfg.Storage.Type != "filesystem" {
		_, _ = fmt.Fprintln(stderr, "serve currently requires filesystem storage")
		return 1
	}
	st := store.NewFilesystem(cfg.Storage.Root)
	h := web.New(web.ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Roots: defaultRoots(), QuietPeriod: cfg.QuietPeriod, Mode: cfg.Mode})
	listener, err := listen("tcp", cfg.Listen)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer listener.Close()
	srv := &http.Server{Handler: h}
	if opts.open {
		opener(localURL(listener.Addr().String()), stderr)
	}
	_, _ = fmt.Fprintf(stdout, "serving agent transcripts on %s\n", listener.Addr())
	go func() { <-ctx.Done(); _ = srv.Close() }()
	err = srv.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func localURL(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "http://127.0.0.1" + listen
	}
	if strings.HasPrefix(listen, "[::]") {
		return "http://127.0.0.1:" + strings.TrimPrefix(listen, "[::]:")
	}
	return "http://" + listen
}

func openBrowser(url string, stderr io.Writer) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		_, _ = fmt.Fprintf(stderr, "open browser: %v\n", err)
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

func runImport(ctx context.Context, args []string, input *os.File, stdout, stderr io.Writer) int {
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
		return emitEligible(ctx, candidate, stdout, stderr)
	}
	interactive := isInteractiveInput(input)
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
		return emitEligible(ctx, candidates[0], stdout, stderr)
	}
	for i, candidate := range candidates {
		_, _ = fmt.Fprintf(stdout, "%d) %s  %s  %s\n", i+1, candidate.Provider, candidate.Project, candidate.Title)
	}
	_, _ = fmt.Fprint(stdout, "Select sessions (comma-separated): ")
	line, err := bufio.NewReader(input).ReadString('\n')
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
		if code := emitEligible(ctx, candidates[index], stdout, stderr); code != 0 {
			return code
		}
	}
	return 0
}

func isInteractiveInput(file *os.File) bool {
	return file != nil && term.IsTerminal(int(file.Fd()))
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

func emitEligible(ctx context.Context, candidate discovery.Candidate, stdout, stderr io.Writer) int {
	reader, facts, err := discovery.OpenEligible(candidate)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer reader.Close()
	svc := library.New(store.NewFilesystem("agent-transcripts-library"), library.AllowLocalQuietEvidence())
	metadata, err := svc.Import(ctx, reader, facts, library.ImportAttrs{
		Destination: session.Directory{Kind: "users", Slug: "local"},
		UploaderKey: "local",
		Title:       candidate.Title,
		Project:     candidate.Project,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, metadata.ID)
	return 0
}

func usage(w io.Writer) int {
	_, _ = fmt.Fprintln(w, "usage: agent-transcripts <serve|import|upload|version|help>")
	return 0
}
