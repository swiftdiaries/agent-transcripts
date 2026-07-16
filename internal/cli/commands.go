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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
	"github.com/swiftdiaries/agent-transcripts/internal/config"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/parser"
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
	return runCommand(deps, ctx, args, input, stdout, stderr, func(ctx context.Context, _ browseOptions) int {
		browseArgs := []string(nil)
		if len(args) > 0 {
			browseArgs = args[1:]
		}
		return runBrowse(ctx, browseArgs, input, stdout, stderr)
	})
}

func runCommand(deps Dependencies, ctx context.Context, args []string, input *os.File, stdout, stderr io.Writer, browse func(context.Context, browseOptions) int) int {
	if len(args) == 0 {
		return browse(ctx, browseOptions{})
	}
	switch args[0] {
	case "help":
		return usage(stdout)
	case "version":
		_, _ = fmt.Fprintf(stdout, "%s %s\n", productName, Version)
		return 0
	case "import":
		return runImportWithLibrary(ctx, args[1:], input, stdout, stderr, deps.Library)
	case "browse":
		opts, err := parseBrowseArgs(args[1:])
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 2
		}
		return browse(ctx, opts)
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
	upload, err := uploadRequest(pkg)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "library package is invalid")
		return 1
	}
	upload.Destination, upload.Title, upload.Description, upload.Tags = opts.destination, opts.title, opts.description, splitTags(opts.tags)
	result, err := deps.upload(ctx, opts.server, upload, token)
	if err != nil {
		// Never include the token in diagnostics, even if a transport error does.
		_, _ = fmt.Fprintln(stderr, "upload failed")
		return 1
	}
	_, _ = fmt.Fprintln(stdout, result.Location)
	return 0
}

func uploadRequest(pkg session.Package) (publish.Request, error) {
	if pkg.SchemaVersion != 2 {
		return publish.Request{SourceName: "transcript.jsonl", Source: bytes.NewReader(pkg.Source)}, nil
	}
	byEntry := make(map[session.SourceEntry][]byte, len(pkg.Sources))
	for _, source := range pkg.Sources {
		byEntry[source.Entry] = source.Bytes
	}
	request := publish.Request{}
	for _, entry := range pkg.SourceManifest.Sources {
		data, ok := byEntry[entry]
		if !ok {
			return publish.Request{}, errors.New("source manifest does not match source blobs")
		}
		switch entry.Role {
		case "main":
			if request.Source != nil {
				return publish.Request{}, errors.New("multiple main sources")
			}
			request.SourceName, request.Source = entry.Name, bytes.NewReader(data)
		case "child":
			request.Children = append(request.Children, publish.ChildSource{SourceName: entry.Name, Source: bytes.NewReader(data)})
		default:
			return publish.Request{}, errors.New("invalid source role")
		}
	}
	if request.Source == nil {
		return publish.Request{}, errors.New("missing main source")
	}
	sort.Slice(request.Children, func(i, j int) bool { return request.Children[i].SourceName < request.Children[j].SourceName })
	return request, nil
}

func splitTags(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

type serveOptions struct {
	configPath  string
	open        bool
	allProjects bool
}

func parseServeArgs(args []string) (serveOptions, error) {
	var got serveOptions
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&got.configPath, "config", "", "configuration file")
	flags.BoolVar(&got.open, "open", false, "open the local browser")
	flags.BoolVar(&got.allProjects, "all-projects", false, "include sessions from all projects")
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
	h, err := serveHandlerWithStoreFactoryForProjects(ctx, cfg, opts.allProjects, makeStore)
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
	return serveHandlerWithStoreFactoryForProjects(ctx, cfg, false, makeStore)
}

func serveHandlerWithStoreFactoryForProjects(ctx context.Context, cfg config.Config, allProjects bool, makeStore func(context.Context, config.Storage) (store.Store, error)) (http.Handler, error) {
	st, err := makeStore(ctx, cfg.Storage)
	if err != nil {
		return nil, err
	}
	base := web.ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Roots: rootsForConfig(cfg), QuietPeriod: cfg.QuietPeriod, Mode: cfg.Mode, AllProjects: allProjects}
	if cfg.Mode == "local" {
		if !allProjects {
			cwd, err := os.Getwd()
			if err != nil {
				return nil, err
			}
			scope, err := discovery.ResolveProjectScope(cwd)
			if err != nil {
				return nil, err
			}
			base.ProjectScope = &scope
		}
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
	path        string
	latest      bool
	provider    string
	limit       int
	allProjects bool
}

func parseImportArgs(args []string) (importOptions, error) {
	var got importOptions
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&got.latest, "latest", false, "select the newest eligible session")
	flags.StringVar(&got.provider, "provider", "", "filter by claude or codex")
	flags.IntVar(&got.limit, "limit", 20, "maximum choices")
	flags.BoolVar(&got.allProjects, "all-projects", false, "include sessions from all projects")
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

type browseOptions struct {
	family      string
	path        string
	latest      bool
	noOpen      bool
	allProjects bool
}

func parseBrowseArgs(args []string) (browseOptions, error) {
	var got browseOptions
	flags := flag.NewFlagSet("browse", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&got.family, "family", "", "eligible family key")
	flags.BoolVar(&got.latest, "latest", false, "select the newest eligible family")
	flags.BoolVar(&got.noOpen, "no-open", false, "do not open a browser")
	flags.BoolVar(&got.allProjects, "all-projects", false, "include sessions from all projects")
	if err := flags.Parse(args); err != nil {
		return got, err
	}
	if flags.NArg() > 1 {
		return got, errors.New("browse accepts at most one path")
	}
	if flags.NArg() == 1 {
		got.path = flags.Arg(0)
	}
	selectors := 0
	for _, set := range []bool{got.family != "", got.latest, got.path != ""} {
		if set {
			selectors++
		}
	}
	if selectors > 1 {
		return got, errors.New("--family, --latest, and a path are mutually exclusive")
	}
	return got, nil
}

func runBrowse(ctx context.Context, args []string, input *os.File, stdout, stderr io.Writer) int {
	return runBrowseWithDeps(ctx, args, input, stdout, stderr, openBrowser, net.Listen, os.Getwd)
}

func runBrowseWithDeps(ctx context.Context, args []string, input *os.File, stdout, stderr io.Writer, opener func(string, io.Writer), listen func(string, string) (net.Listener, error), getwd func() (string, error)) int {
	opts, err := parseBrowseArgs(args)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	var families []discovery.SessionFamilyCandidate
	if opts.path != "" {
		family, err := familyForPath(ctx, opts.path)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		families = []discovery.SessionFamilyCandidate{family}
	} else {
		families, err = discoverCommandFamilies(ctx, opts.allProjects, getwd)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
	}
	selected, ok := selectFamily(families, opts.family, opts.latest)
	if opts.path != "" && len(families) == 1 {
		selected, ok = families[0], true
	}
	if !ok && opts.family != "" {
		_, _ = fmt.Fprintln(stderr, "selected family is no longer available")
		return 1
	}
	if !ok {
		if !isInteractiveInput(input) {
			_, _ = fmt.Fprintln(stderr, "non-interactive browse requires --family, --latest, or a path")
			return 2
		}
		selected, ok = pickFamily(input, stdout, families, opts.allProjects)
		if !ok {
			_, _ = fmt.Fprintln(stderr, "no eligible completed sessions")
			return 1
		}
	}
	listener, err := listen("tcp", "127.0.0.1:0")
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	defer listener.Close()
	h := web.New(web.ServerConfig{FocusedFamily: selected})
	url := localURL(listener.Addr().String()) + "/live/" + selected.Provider + "/" + selected.ProviderSessionID
	if !opts.noOpen {
		opener(url, stderr)
	}
	_, _ = fmt.Fprintf(stdout, "browsing agent transcript on %s\n", listener.Addr())
	srv := &http.Server{Handler: h}
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

func discoverCommandFamilies(ctx context.Context, allProjects bool, getwd func() (string, error)) ([]discovery.SessionFamilyCandidate, error) {
	if allProjects {
		return discovery.DiscoverAllFamilies(ctx, defaultRoots(), time.Now(), 5*time.Minute)
	}
	cwd, err := getwd()
	if err != nil {
		return nil, err
	}
	scope, err := discovery.ResolveProjectScope(cwd)
	if err != nil {
		return nil, err
	}
	return discovery.DiscoverFamilies(ctx, defaultRoots(), scope, time.Now(), 5*time.Minute)
}

func selectFamily(families []discovery.SessionFamilyCandidate, key string, latest bool) (discovery.SessionFamilyCandidate, bool) {
	if latest && len(families) > 0 {
		return families[0], true
	}
	if key != "" {
		for _, family := range families {
			if family.Key == key {
				return family, true
			}
		}
	}
	return discovery.SessionFamilyCandidate{}, false
}

func pickFamily(input *os.File, stdout io.Writer, families []discovery.SessionFamilyCandidate, allProjects bool) (discovery.SessionFamilyCandidate, bool) {
	if len(families) == 0 {
		return discovery.SessionFamilyCandidate{}, false
	}
	reader := bufio.NewReader(input)
	if allProjects {
		projects := make([]string, 0)
		seen := map[string]bool{}
		for _, family := range families {
			if !seen[family.Project.Key] {
				projects = append(projects, family.Project.Key)
				seen[family.Project.Key] = true
			}
		}
		for i, key := range projects {
			_, _ = fmt.Fprintf(stdout, "%d) %s\n", i+1, key)
		}
		_, _ = fmt.Fprint(stdout, "Select project: ")
		line, _ := reader.ReadString('\n')
		indexes, err := parseSelections(line, len(projects))
		if err != nil || len(indexes) != 1 {
			return discovery.SessionFamilyCandidate{}, false
		}
		chosen := projects[indexes[0]]
		filtered := families[:0]
		for _, family := range families {
			if family.Project.Key == chosen {
				filtered = append(filtered, family)
			}
		}
		families = filtered
	}
	for i, family := range families {
		_, _ = fmt.Fprintf(stdout, "%d) %s  %s\n", i+1, family.Project.DisplayName, family.Title)
	}
	_, _ = fmt.Fprint(stdout, "Select session: ")
	line, _ := reader.ReadString('\n')
	indexes, err := parseSelections(line, len(families))
	if err != nil || len(indexes) != 1 {
		return discovery.SessionFamilyCandidate{}, false
	}
	return families[indexes[0]], true
}

func familyForPath(ctx context.Context, path string) (discovery.SessionFamilyCandidate, error) {
	candidate, err := discovery.InspectPath(ctx, path, time.Now(), 5*time.Minute)
	if err != nil {
		return discovery.SessionFamilyCandidate{}, err
	}
	root := filepath.Dir(candidate.Path)
	roots := discovery.Roots{Claude: []string{root}, Codex: []string{root}}
	candidates, err := discovery.Discover(ctx, roots, time.Now(), 5*time.Minute)
	if err != nil {
		return discovery.SessionFamilyCandidate{}, err
	}
	f, err := os.Open(candidate.Path)
	if err != nil {
		return discovery.SessionFamilyCandidate{}, err
	}
	parsed, parseErr := parser.DefaultRegistry().DetectAndParse(ctx, f)
	closeErr := f.Close()
	if parseErr != nil {
		return discovery.SessionFamilyCandidate{}, parseErr
	}
	if closeErr != nil {
		return discovery.SessionFamilyCandidate{}, closeErr
	}
	if parsed.WorkingDirectory == "" {
		return discovery.SessionFamilyCandidate{}, errors.New("selected session has no working directory")
	}
	scope, err := discovery.ResolveProjectScope(parsed.WorkingDirectory)
	if err != nil {
		return discovery.SessionFamilyCandidate{}, err
	}
	families, err := discovery.FormFamilies(candidates, scope)
	if err != nil {
		return discovery.SessionFamilyCandidate{}, err
	}
	for _, family := range families {
		if filepath.Clean(family.Main.Path) == filepath.Clean(candidate.Path) {
			return family, nil
		}
	}
	return discovery.SessionFamilyCandidate{}, errors.New("selected session is no longer available")
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
		family, err := familyForPath(ctx, opts.path)
		if err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		if opts.provider != "" && family.Provider != opts.provider {
			_, _ = fmt.Fprintln(stderr, "source does not match --provider")
			return 1
		}
		return emitFamilyWithLibrary(ctx, family, stdout, stderr, libraryStore)
	}
	interactive := isInteractiveInput(input)
	if !opts.latest && !interactive {
		_, _ = fmt.Fprintln(stderr, "non-interactive import requires a path or --latest")
		return 2
	}
	families, err := discoverCommandFamilies(ctx, opts.allProjects, os.Getwd)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	families = filterFamilies(families, opts)
	if len(families) == 0 {
		_, _ = fmt.Fprintln(stderr, "no eligible completed sessions")
		return 1
	}
	if opts.latest {
		return emitFamilyWithLibrary(ctx, families[0], stdout, stderr, libraryStore)
	}
	if opts.allProjects {
		selected, ok := pickFamily(input, stdout, families, true)
		if !ok {
			_, _ = fmt.Fprintln(stderr, "no sessions selected")
			return 2
		}
		return emitFamilyWithLibrary(ctx, selected, stdout, stderr, libraryStore)
	}
	for i, family := range families {
		_, _ = fmt.Fprintf(stdout, "%d) %s  %s  %s\n", i+1, family.Provider, family.Project.DisplayName, family.Title)
	}
	_, _ = fmt.Fprint(stdout, "Select sessions (comma-separated): ")
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	indexes, err := parseSelections(line, len(families))
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	for _, index := range indexes {
		if code := emitFamilyWithLibrary(ctx, families[index], stdout, stderr, libraryStore); code != 0 {
			return code
		}
	}
	return 0
}

func filterFamilies(families []discovery.SessionFamilyCandidate, opts importOptions) []discovery.SessionFamilyCandidate {
	filtered := make([]discovery.SessionFamilyCandidate, 0, len(families))
	for _, family := range families {
		if opts.provider == "" || family.Provider == opts.provider {
			filtered = append(filtered, family)
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

func emitFamilyWithLibrary(ctx context.Context, family discovery.SessionFamilyCandidate, stdout, stderr io.Writer, libraryStore store.Store) int {
	snapshot, err := discovery.SnapshotFamily(family)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	svc := library.New(libraryStore, library.AllowLocalQuietEvidence())
	metadata, _, err := svc.ImportFamilyWithStatus(ctx, snapshot, library.ImportAttrs{
		Destination: session.Directory{Kind: "users", Slug: "local"}, UploaderKey: "local", Title: family.Title, Project: family.Project.DisplayName,
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, metadata.ID)
	return 0
}

func usage(w io.Writer) int {
	_, _ = fmt.Fprintln(w, "usage: agent-transcripts [browse [--family key|--latest|--no-open|path] [--all-projects]] | <serve|import|upload|version|help>")
	return 0
}
