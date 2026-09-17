// Package openpath resolves which PDF file the "open" action/link-handler
// should open, in priority order: argv, then clicked-URL env, then context
// JSON's clicked_url/selected_text.
package openpath

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Context mirrors the fields this package reads out of
// $HERDR_PLUGIN_CONTEXT_JSON; herdr documents more fields than this, but
// only these are decoded.
type Context struct {
	ClickedURL     string `json:"clicked_url"`
	SelectedText   string `json:"selected_text"`
	FocusedPaneCwd string `json:"focused_pane_cwd"`
}

// stripFileURL turns a file:// URL into a filesystem path. Percent-decoding
// applies only here: a bare path is taken literally, so a filename that
// really contains "%20" still opens.
func stripFileURL(candidate string) (string, error) {
	if !strings.HasPrefix(candidate, "file://") {
		return candidate, nil
	}
	u, err := url.Parse(candidate)
	if err != nil {
		return "", fmt.Errorf("invalid file URL %q: %w", candidate, err)
	}
	// A host other than localhost names a file on another machine, which
	// this process cannot open.
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", fmt.Errorf("file URL names remote host %q, which cannot be opened locally", u.Host)
	}
	dec, derr := url.PathUnescape(u.Path)
	if derr != nil {
		return u.Path, nil
	}
	return dec, nil
}

// Resolve picks the PDF path to open, in priority order: argvPath,
// clickedURLEnv ($HERDR_PLUGIN_CLICKED_URL), then the context JSON's
// clicked_url, then its selected_text. A relative result is joined against
// the context's focused_pane_cwd (falling back to cwd) and made absolute.
// Malformed or empty contextJSON is treated as "no context", not an error.
// This function only computes a path; it does not check the file exists.
func Resolve(argvPath, clickedURLEnv, contextJSON, cwd string) (string, error) {
	candidate := argvPath
	if candidate == "" {
		candidate = clickedURLEnv
	}

	var ctx Context
	if contextJSON != "" {
		_ = json.Unmarshal([]byte(contextJSON), &ctx)
	}

	if candidate == "" {
		candidate = ctx.ClickedURL
	}
	if candidate == "" {
		candidate = ctx.SelectedText
	}
	if candidate == "" {
		return "", fmt.Errorf("no candidate path found (no arg, no clicked_url, no selected_text)")
	}

	// Only the trailing newline a shell or clipboard adds is stripped;
	// spaces are legal at either end of a filename.
	candidate = strings.TrimRight(candidate, "\r\n")

	candidate, err := stripFileURL(candidate)
	if err != nil {
		return "", err
	}
	if candidate == "" {
		return "", fmt.Errorf("candidate resolved to empty string")
	}
	if strings.ContainsRune(candidate, 0) {
		return "", fmt.Errorf("非法路徑（含 NUL 位元組）: %q", candidate)
	}

	if !filepath.IsAbs(candidate) {
		base := ctx.FocusedPaneCwd
		if base == "" {
			base = cwd
		}
		candidate = filepath.Join(base, candidate)
	}

	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("make absolute %q: %w", candidate, err)
	}
	return abs, nil
}
