// Command herdr-pdf is the herdr PDF Viewer plugin's single binary. It
// implements two entry points declared in herdr-plugin.toml:
//
//	view   the [[panes]] entrypoint — takes over its pane and renders PDFVIEW_FILE
//	open   the [[actions]]/link_handler entrypoint — resolves a path and opens a "view" pane
//
// plus a "version" subcommand for build/support diagnostics.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/tim80411/herdr-pdf/internal/openpath"
	"github.com/tim80411/herdr-pdf/internal/viewer"
)

// version is bumped independently of herdr-plugin.toml's manifest version;
// it identifies this binary for viewer.log / bug reports. A release build
// overrides it with -ldflags "-X main.version=...".
var version = "0.3.2"

const usage = `usage: herdr-pdf <command> [args]

commands:
  view [path]                        run the PDF viewer in the current pane (reads $PDFVIEW_FILE)
  open [--fallback=system] [path]    resolve a PDF path and open it in a new "view" pane;
                                      --fallback=system hands a non-.pdf path to the OS opener instead of erroring
  version                            print build info`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "view":
		os.Exit(runView(os.Args[2:]))
	case "open":
		os.Exit(runOpen(os.Args[2:]))
	case "version", "-v", "--version":
		fmt.Printf("herdr-pdf %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
	case "-h", "--help", "help":
		fmt.Println(usage)
	default:
		fmt.Fprintf(os.Stderr, "herdr-pdf: unknown command %q\n\n%s\n", os.Args[1], usage)
		os.Exit(2)
	}
}

// runView is the [[panes]] "view" entrypoint. It never returns a nonzero
// exit code for "expected" problems (missing file, missing HERDR_PANE_ID, a
// PDF that won't open) — viewer.Run() shows a usage screen in the pane
// itself for those instead of leaving it looking like a crash.
func runView(args []string) int {
	pdfPath := os.Getenv("PDFVIEW_FILE")
	if pdfPath == "" && len(args) > 0 {
		pdfPath = args[0]
	}
	transport, legacyTransport := viewer.ParseTransport(os.Getenv("HERDR_PDF_TRANSPORT"))
	opts := viewer.Options{
		PDFPath: pdfPath,
		// Where browser mode starts when no file was named. Nothing expands
		// "~" on the way in: the value arrives through --env or a manifest,
		// neither of which goes through a shell.
		StartDir: expandHome(os.Getenv("PDFVIEW_DIR")),
		PaneID:   os.Getenv("HERDR_PANE_ID"),
		LogPath:  viewer.LogPath(),
		// Both knobs are read here rather than inside the viewer package so
		// Run() stays testable without a real pane or a mutated environment.
		Transport:   transport,
		Trace:       isTruthy(os.Getenv("HERDR_PDF_TRACE")),
		MaxFrameKB:  atoiDefault(os.Getenv("HERDR_PDF_MAX_FRAME_KB"), 0),
		Progressive: tristate(os.Getenv("HERDR_PDF_PROGRESSIVE")),
	}
	if legacyTransport {
		opts.LegacyTransport = os.Getenv("HERDR_PDF_TRANSPORT")
	}
	if err := viewer.New(opts).Run(); err != nil {
		// A termination signal is a clean stop, reported with the status a
		// shell expects rather than a generic failure.
		var sigErr *viewer.ExitSignal
		if errors.As(err, &sigErr) {
			return sigErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "herdr-pdf view:", err)
		return 1
	}
	return 0
}

// tuningEnvNames are the viewer knobs forwarded into any pane this process
// opens. `herdr plugin pane open` builds the new pane's environment from
// the manifest and --env flags, not from this process's environment, so
// without forwarding an operator setting HERDR_PDF_TRANSPORT in their shell
// would see it silently ignored by the pane they just opened.
var tuningEnvNames = []string{"HERDR_PDF_TRANSPORT", "HERDR_PDF_TRACE", "HERDR_PDF_MAX_FRAME_KB", "HERDR_PDF_PROGRESSIVE"}

// expandHome resolves a leading "~". Nothing expands it on the way in: the
// value arrives through --env or a manifest, neither of which goes through
// a shell.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
}

// openOpts is runOpen's argv, parsed out into a plain struct so the parsing
// rules are testable without a real os.Args.
type openOpts struct {
	fallback string // "" (no fallback) or "system"
	path     string
}

// parseOpenArgs reads open's argv. herdr-file-viewer's `O` key spawns the
// configured opener with the selected file's absolute path appended last,
// so the path is normally the final argument — but someone typing the
// command by hand may put it first, so flag position isn't fixed: every
// "--"-prefixed token is treated as a flag and the last remaining token is
// the path. Only --fallback=system and --fallback=none (equivalent to
// omitting the flag) are recognised; anything else is an error.
func parseOpenArgs(args []string) (openOpts, error) {
	var opts openOpts
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			opts.path = a
			continue
		}
		switch a {
		case "--fallback=system":
			opts.fallback = "system"
		case "--fallback=none":
			opts.fallback = ""
		default:
			return openOpts{}, fmt.Errorf("unknown flag %q", a)
		}
	}
	return opts, nil
}

// systemOpenerArgv picks the argv for the platform's file-association
// opener, used to hand off a resolved path that isn't a .pdf instead of
// rejecting it outright.
func systemOpenerArgv(goos, path string) ([]string, error) {
	switch goos {
	case "darwin":
		return []string{"open", path}, nil
	case "linux":
		return []string{"xdg-open", path}, nil
	default:
		return nil, fmt.Errorf("no system opener known for GOOS %q", goos)
	}
}

// runOpen is the [[actions]] "open" / link_handler entrypoint. It resolves
// a path (see internal/openpath) and execs `herdr plugin pane open` to
// launch a "view" pane, rather than spawning a child, so this short-lived
// process doesn't linger once the real viewer is launched.
func runOpen(args []string) int {
	opts, perr := parseOpenArgs(args)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: %v\n\n%s\n", perr, usage)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	contextJSON := os.Getenv("HERDR_PLUGIN_CONTEXT_JSON")
	resolved, err := openpath.Resolve(
		opts.path,
		os.Getenv("HERDR_PLUGIN_CLICKED_URL"),
		contextJSON,
		cwd,
	)
	// Nothing offered at all — a keybinding pressed with no selection, which
	// is the usual case since a selection made in the outer terminal
	// emulator never reaches herdr. Browse instead of erroring out.
	if errors.Is(err, openpath.ErrNoCandidate) {
		return openBrowser(openpath.ParseContext(contextJSON), cwd)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: could not resolve a PDF path: %v\n", err)
		return 1
	}
	// Checked here rather than in openpath.Resolve: resolution is shared
	// with callers that legitimately handle other files, but this entrypoint
	// only ever launches the PDF viewer itself.
	if !strings.EqualFold(filepath.Ext(resolved), ".pdf") {
		if opts.fallback == "system" {
			return execSystemOpener(resolved)
		}
		fmt.Fprintf(os.Stderr, "herdr-pdf open: not a .pdf file: %s\n", resolved)
		return 1
	}
	if fi, statErr := os.Stat(resolved); statErr != nil || fi.IsDir() {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: resolved path does not exist: %s\n", resolved)
		return 1
	}

	cmdArgs := []string{
		"plugin", "pane", "open",
		"--plugin", "pdf",
		"--entrypoint", "view",
		"--placement", "split",
		"--direction", "right",
		"--env", "PDFVIEW_FILE=" + resolved,
		"--focus",
	}
	cmdArgs = appendTuningEnv(cmdArgs)
	if paneID := os.Getenv("HERDR_PANE_ID"); paneID != "" {
		cmdArgs = append(cmdArgs, "--target-pane", paneID)
	}
	return execHerdr("open", cmdArgs)
}

// openBrowser is runOpen's fallback: the same "view" pane, opened with a
// directory instead of a file so the viewer comes up in browser mode. There
// is no separate picker entrypoint — browsing and reading are two modes of
// one pane, so choosing a file does not have to open a second one.
func openBrowser(ctx openpath.Context, cwd string) int {
	dir := ctx.FocusedPaneCwd
	if dir == "" {
		dir = ctx.WorkspaceCwd
	}
	if dir == "" {
		dir = cwd
	}
	cmdArgs := []string{
		"plugin", "pane", "open",
		"--plugin", "pdf",
		"--entrypoint", "view",
		"--placement", "split",
		"--direction", "right",
		"--env", "PDFVIEW_DIR=" + dir,
		"--focus",
	}
	if ctx.FocusedPaneID != "" {
		cmdArgs = append(cmdArgs, "--target-pane", ctx.FocusedPaneID)
	}
	cmdArgs = appendTuningEnv(cmdArgs)
	return execHerdr("open", cmdArgs)
}

// appendTuningEnv adds --env flags for whichever tuning knobs are set here.
func appendTuningEnv(cmdArgs []string) []string {
	for _, name := range tuningEnvNames {
		if val := os.Getenv(name); val != "" {
			cmdArgs = append(cmdArgs, "--env", name+"="+val)
		}
	}
	return cmdArgs
}

// execHerdr replaces this process with a herdr CLI invocation, so the
// short-lived action process doesn't linger behind the pane it opened.
// args excludes argv[0]. It returns only on failure.
func execHerdr(what string, args []string) int {
	herdrBin := os.Getenv("HERDR_BIN_PATH")
	if herdrBin == "" {
		herdrBin = "herdr"
	}
	binPath, lerr := exec.LookPath(herdrBin)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf %s: cannot find %q in PATH: %v\n", what, herdrBin, lerr)
		return 1
	}
	if execErr := syscall.Exec(binPath, append([]string{herdrBin}, args...), os.Environ()); execErr != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf %s: exec %s failed: %v\n", what, binPath, execErr)
		return 1
	}
	return 0 // unreachable when exec succeeds
}

// execSystemOpener hands resolved off to the platform's file-association
// opener, exec'd in place of this process like execHerdr does for the herdr
// CLI. Only reached with --fallback=system and a non-.pdf path; unlike the
// .pdf path, resolved is not stat'd first, so a directory reaches the OS
// opener too rather than being rejected as "does not exist".
func execSystemOpener(resolved string) int {
	argv, aerr := systemOpenerArgv(runtime.GOOS, resolved)
	if aerr != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: %v\n", aerr)
		return 1
	}
	binPath, lerr := exec.LookPath(argv[0])
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: cannot find %q in PATH: %v\n", argv[0], lerr)
		return 1
	}
	if execErr := syscall.Exec(binPath, argv, os.Environ()); execErr != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: exec %s failed: %v\n", binPath, execErr)
		return 1
	}
	return 0 // unreachable when exec succeeds
}

// isTruthy accepts the handful of spellings people actually type for an
// on/off environment variable, so HERDR_PDF_TRACE=true works as well as =1.
func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// atoiDefault parses a positive integer environment variable, falling back
// to def for anything unparseable so a bad value cannot make the viewer
// encode every page down to nothing.
func atoiDefault(v string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// tristate distinguishes "unset" from "explicitly off", which a plain bool
// cannot: the viewer defaults progressive refinement on for a remote
// session and off for a local one, and only an explicit value overrides
// that. Anything unrecognised is treated as unset.
func tristate(v string) *bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		t := true
		return &t
	case "0", "false", "no", "off":
		f := false
		return &f
	}
	return nil
}
