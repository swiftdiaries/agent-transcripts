package cli

import (
	"bufio"
	"bytes"
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

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
	"github.com/swiftdiaries/agent-transcripts/internal/config"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/publish"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
	"github.com/swiftdiaries/agent-transcripts/internal/web"
	"golang.org/x/term"
)

const Version = "dev"

const productName = "agent-transcripts"

// Dependencies contains the production collaborators used by process-facing
// commands. It permits an embedding application to compose the CLI with its
// own store without replacing the application services it calls.
type Dependencies struct {
	Library store.Store
}

// DefaultDependencies provides the filesystem-backed composition used by the
// standalone binary.
func DefaultDependencies() Dependencies {
	return Dependencies{Library: store.NewFilesystem("agent-transcripts-library")}
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return DefaultDependencies().Run(ctx, args, os.Stdin, stdout, stderr)
}

// Run executes the command using the supplied production collaborators.
func (deps Dependencies) Run(ctx context.Context, args []string, input *os.File, stdout, stderr io.Writer) int {
	if deps.Library == nil {
		deps.Library = DefaultDependencies().Library
	}
	if len(args) == 0 {
		return usage(stdout)
	}
	switch args[0] {
	case "help":
		return usage(stdout)
	case "version":
		_, _ = fmt.Fprintf(stdout, "%s %s\n", productName, Version)
		return 0
	case "import":
		return runImportWithLibrary(ctx, args[1:], input, stdout, stderr, deps.Library)
	case "serve":
		return runServe(ctx, args[1:], stdout, stderr)
	case "upload":
		return runUploadWithDeps(ctx, args[1:], input, stdout, stderr, uploadDeps{
			library:     deps.Library,
			interactive: isInteractiveInput,
			getenv:      os.Getenv,
			readPassword: func(fd int) ([]byte, error) {
				return term.ReadPassword(fd)
			},
			upload: func(ctx context.Context, server string, req publish.Request, token string) (publish.Result, error) {
				return (publish.Client{BaseURL: server, Token: token}).Upload(ctx, req)
			},
		})
	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

type uploadOptions struct {
	server, destination, title, description string
	tags                                    string
	yes                                     bool
}

func parseUploadArgs(args []string) (uploadOptions, string, error) {
	var got uploadOptions
	flags := flag.NewFlagSet("upload", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&got.server, "server", "", "hosted server URL")
	flags.StringVar(&got.destination, "destination", "", "users or projects destination")
	flags.StringVar(&got.title, "title", "", "optional title")
	flags.StringVar(&got.description, "description", "", "optional description")
	flags.StringVar(&got.tags, "tags", "", "comma-separated tags")
	flags.BoolVar(&got.yes, "yes", false, "confirm publishing without a prompt")
	if err := flags.Parse(args); err != nil {
		return got, "", err
	}
	if got.server == "" || got.destination == "" || flags.NArg() != 1 {
		return got, "", errors.New("upload requires --server, --destination, and a library package ID")
	}
	if _, err := parseUploadDestination(got.destination); err != nil {
		return got, "", err
	}
	return got, flags.Arg(0), nil
}

func parseUploadDestination(value string) (session.Directory, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return session.Directory{}, errors.New("destination must be users/<slug> or projects/<slug>")
	}
	d := session.Directory{Kind: parts[0], Slug: parts[1]}
	if err := session.ValidateDirectory(d); err != nil {
		return session.Directory{}, errors.New("destination must be users/<slug> or projects/<slug>")
	}
	return d, nil
}

func runUpload(ctx context.Context, args []string, input *os.File, stdout, stderr io.Writer) int {
	return DefaultDependencies().Run(ctx, append([]string{"upload"}, args...), input, stdout, stderr)
}

// uploadDeps keeps terminal and transport effects injectable for tests while
// the command itself remains the only code that reads credentials.
type uploadDeps struct {
	library      store.Store
	interactive  func(*os.File) bool
	getenv       func(string) string
	readPassword func(int) ([]byte, error)
	upload       func(context.Context, string, publish.Request, string) (publish.Result, error)
}

func runUploadWithDeps(ctx context.Context, args []string, input *os.File, stdout, stderr io.Writer, deps uploadDeps) int {
	opts, id, err := parseUploadArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	if !opts.yes {
		if !deps.interactive(input) {
			_, _ = fmt.Fprintln(stderr, "non-interactive upload requires --yes")
			return 2
		}
		_, _ = fmt.Fprintf(stdout, "Publish %s to %s? [y/N] ", id, opts.destination)
		answer, err := bufio.NewReader(input).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if strings.ToLower(strings.TrimSpace(answer)) != "y" && strings.ToLower(strings.TrimSpace(answer)) != "yes" {
			_, _ = fmt.Fprintln(stderr, "upload cancelled")
			return 1
		}
	}
	token := strings.TrimSpace(deps.getenv("AGENT_TRANSCRIPTS_TOKEN"))
	if token == "" {
		if !deps.interactive(input) {
			_, _ = fmt.Fprintln(stderr, "AGENT_TRANSCRIPTS_TOKEN is required for non-interactive upload")
			return 2
		}
		_, _ = fmt.Fprint(stderr, "Bearer token: ")
		value, err := deps.readPassword(int(input.Fd()))
		_, _ = fmt.Fprintln(stderr)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, "could not read bearer token")
			return 1
		}
		token = strings.TrimSpace(string(value))
	}
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "bearer token is required")
		return 2
	}
	pkg, err := deps.library.GetSession(ctx, id)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "library package not found")
		return 1
	}
	result, err := deps.upload(ctx, opts.server, publish.Request{
		SourceName: "transcript.jsonl", Source: bytes.NewReader(pkg.Source), Destination: opts.destination,
		Title: opts.title, Description: opts.description, Tags: splitTags(opts.tags),
	}, token)
	if err != nil {
		// Never include the token in diagnostics, even if a transport error does.
		_, _ = fmt.Fprintln(stderr, "upload failed")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, result.Location)
	return 0
}

func splitTags(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
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
	return runServeWithDepsAndStoreFactory(ctx, args, stdout, stderr, opener, listen, productionStoreForConfig)
}

func runServeWithDepsAndStoreFactory(ctx context.Context, args []string, stdout, stderr io.Writer, opener func(string, io.Writer), listen func(string, string) (net.Listener, error), makeStore func(context.Context, config.Storage) (store.Store, error)) int {
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
	h, err := serveHandlerWithStoreFactory(ctx, cfg, makeStore)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "invalid server authentication configuration")
		return 1
	}
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

func serveHandler(cfg config.Config) (http.Handler, error) {
	return serveHandlerWithStoreFactory(context.Background(), cfg, productionStoreForConfig)
}

func serveHandlerWithStoreFactory(ctx context.Context, cfg config.Config, makeStore func(context.Context, config.Storage) (store.Store, error)) (http.Handler, error) {
	st, err := makeStore(ctx, cfg.Storage)
	if err != nil {
		return nil, err
	}
	base := web.ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Roots: rootsForConfig(cfg), QuietPeriod: cfg.QuietPeriod, Mode: cfg.Mode}
	if cfg.Mode == "local" {
		return web.New(base), nil
	}
	csrf, err := auth.NewCSRF(cfg.CookieKeys[0], cfg.ExternalOrigin)
	if err != nil {
		return nil, err
	}
	tokens, err := auth.NewTokenManager(cfg.TokenKey)
	if err != nil {
		return nil, err
	}
	switch cfg.Auth.Type {
	case "proxy":
		cidrs, err := auth.ParseCIDRs(cfg.TrustedProxyCIDRs)
		if err != nil {
			return nil, err
		}
		base.Provider = auth.NewProxy(cfg.Auth.Proxy.UserHeader, cfg.Auth.Proxy.NameHeader, cidrs)
	case "oidc":
		redirect := cfg.Auth.OIDC.RedirectURL
		if redirect == "" {
			redirect = strings.TrimSuffix(cfg.ExternalOrigin, "/") + "/auth/callback"
		}
		provider, err := auth.NewOIDC(auth.OIDCConfig{Issuer: cfg.Auth.OIDC.Issuer, ClientID: cfg.Auth.OIDC.ClientID, ClientSecret: cfg.Auth.OIDC.ClientSecret, RedirectURL: redirect, CookieKeys: cfg.CookieKeys})
		if err != nil {
			return nil, err
		}
		base.Provider = provider
	default:
		return nil, errors.New("unsupported hosted auth")
	}
	base.CSRF, base.Tokens = csrf, tokens
	return web.New(base), nil
}

func productionStoreForConfig(ctx context.Context, cfg config.Storage) (store.Store, error) {
	if cfg.Type == "filesystem" {
		return store.NewFilesystem(cfg.Root), nil
	}
	client, err := store.NewAWSS3(ctx, cfg.Region, cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	return store.NewS3(client, cfg.Bucket, cfg.Prefix), nil
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
	return runImportWithLibrary(ctx, args, input, stdout, stderr, DefaultDependencies().Library)
}

func runImportWithLibrary(ctx context.Context, args []string, input *os.File, stdout, stderr io.Writer, libraryStore store.Store) int {
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
		return emitEligibleWithLibrary(ctx, candidate, stdout, stderr, libraryStore)
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
		return emitEligibleWithLibrary(ctx, candidates[0], stdout, stderr, libraryStore)
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
		if code := emitEligibleWithLibrary(ctx, candidates[index], stdout, stderr, libraryStore); code != 0 {
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

// rootsForConfig uses configured roots for both adapters. Each adapter applies
// its provider-specific filename and parser checks, so a shared root supports a
// combined archive without trusting a path name to identify its provider.
func rootsForConfig(cfg config.Config) discovery.Roots {
	if len(cfg.SourceRoots) == 0 {
		return defaultRoots()
	}
	roots := append([]string(nil), cfg.SourceRoots...)
	return discovery.Roots{Claude: roots, Codex: roots}
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
	return emitEligibleWithLibrary(ctx, candidate, stdout, stderr, DefaultDependencies().Library)
}

func emitEligibleWithLibrary(ctx context.Context, candidate discovery.Candidate, stdout, stderr io.Writer, libraryStore store.Store) int {
	reader, facts, err := discovery.OpenEligible(candidate)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer reader.Close()
	svc := library.New(libraryStore, library.AllowLocalQuietEvidence())
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
