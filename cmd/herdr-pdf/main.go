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
var version = "0.2.0"

const usage = `usage: herdr-pdf <command> [args]

commands:
  view [path]   run the PDF viewer in the current pane (reads $PDFVIEW_FILE)
  open [path]   resolve a PDF path and open it in a new "view" pane
  version       print build info`

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
	opts := viewer.Options{
		PDFPath: pdfPath,
		PaneID:  os.Getenv("HERDR_PANE_ID"),
		LogPath: viewer.LogPath(),
		// Both knobs are read here rather than inside the viewer package so
		// Run() stays testable without a real pane or a mutated environment.
		Transport:   viewer.ParseTransport(os.Getenv("HERDR_PDF_TRANSPORT")),
		Trace:       isTruthy(os.Getenv("HERDR_PDF_TRACE")),
		MaxFrameKB:  atoiDefault(os.Getenv("HERDR_PDF_MAX_FRAME_KB"), 0),
		Progressive: tristate(os.Getenv("HERDR_PDF_PROGRESSIVE")),
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

// runOpen is the [[actions]] "open" / link_handler entrypoint. It resolves
// a path (see internal/openpath) and execs `herdr plugin pane open` to
// launch a "view" pane, rather than spawning a child, so this short-lived
// process doesn't linger once the real viewer is launched.
func runOpen(args []string) int {
	argvPath := ""
	if len(args) > 0 {
		argvPath = args[0]
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}

	resolved, err := openpath.Resolve(
		argvPath,
		os.Getenv("HERDR_PLUGIN_CLICKED_URL"),
		os.Getenv("HERDR_PLUGIN_CONTEXT_JSON"),
		cwd,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: could not resolve a PDF path: %v\n", err)
		return 1
	}
	// Checked here rather than in openpath.Resolve: resolution is shared
	// with callers that legitimately handle other files, but this entrypoint
	// only ever launches the PDF viewer.
	if !strings.EqualFold(filepath.Ext(resolved), ".pdf") {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: not a .pdf file: %s\n", resolved)
		return 1
	}
	if fi, statErr := os.Stat(resolved); statErr != nil || fi.IsDir() {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: resolved path does not exist: %s\n", resolved)
		return 1
	}

	herdrBin := os.Getenv("HERDR_BIN_PATH")
	if herdrBin == "" {
		herdrBin = "herdr"
	}
	binPath, lerr := exec.LookPath(herdrBin)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "herdr-pdf open: cannot find %q in PATH: %v\n", herdrBin, lerr)
		return 1
	}

	cmdArgs := []string{
		herdrBin, "plugin", "pane", "open",
		"--plugin", "pdf",
		"--entrypoint", "view",
		"--placement", "split",
		"--direction", "right",
		"--env", "PDFVIEW_FILE=" + resolved,
		"--focus",
	}
	// Forward the viewer's own tuning knobs into the new pane. `herdr
	// plugin pane open` builds the pane's environment from the manifest and
	// these --env flags, not from this process's environment, so without
	// this an operator setting HERDR_PDF_TRANSPORT in their shell would see
	// it silently ignored by the pane they just opened.
	for _, name := range []string{"HERDR_PDF_TRANSPORT", "HERDR_PDF_TRACE", "HERDR_PDF_MAX_FRAME_KB", "HERDR_PDF_PROGRESSIVE"} {
		if val := os.Getenv(name); val != "" {
			cmdArgs = append(cmdArgs, "--env", name+"="+val)
		}
	}
	if paneID := os.Getenv("HERDR_PANE_ID"); paneID != "" {
		cmdArgs = append(cmdArgs, "--target-pane", paneID)
	}

	if execErr := syscall.Exec(binPath, cmdArgs, os.Environ()); execErr != nil {
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
