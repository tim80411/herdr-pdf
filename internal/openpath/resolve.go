// Package openpath resolves which PDF file the "open" action/link-handler
// should open, porting resolve_path.py's priority order and semantics
// exactly (including its one deliberately narrow quirk: a bare, non-
// file:// candidate is percent-decoded only when it starts with "%").
package openpath

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// Context mirrors the fields this package reads out of
// $HERDR_PLUGIN_CONTEXT_JSON. herdr documents more fields than this; only
// the ones resolve_path.py used are decoded here.
type Context struct {
	ClickedURL     string `json:"clicked_url"`
	SelectedText   string `json:"selected_text"`
	FocusedPaneCwd string `json:"focused_pane_cwd"`
}

// stripFileURL strips a file:// prefix and percent-decodes the result, or
// percent-decodes a bare candidate that itself starts with "%". Ports
// resolve_path.py's strip_file_url() verbatim, quirk included: a %-escape
// in the *middle* of a bare (non-file://) path is deliberately left alone.
func stripFileURL(candidate string) string {
	if strings.HasPrefix(candidate, "file://") {
		u, err := url.Parse(candidate)
		if err != nil {
			return candidate
		}
		if dec, derr := url.PathUnescape(u.Path); derr == nil {
			return dec
		}
		return u.Path
	}
	if strings.HasPrefix(candidate, "%") {
		if dec, err := url.PathUnescape(candidate); err == nil {
			return dec
		}
	}
	return candidate
}

// Resolve picks the PDF path to open, in priority order: argvPath,
// clickedURLEnv ($HERDR_PLUGIN_CLICKED_URL), then the context JSON's
// clicked_url, then its selected_text. A relative result is joined against
// the context's focused_pane_cwd, falling back to cwd, and made absolute.
//
// contextJSON is the raw value of $HERDR_PLUGIN_CONTEXT_JSON; malformed or
// empty JSON is treated as "no context" (matching resolve_path.py's
// try/except that swallows JSONDecodeError/ValueError), not an error.
//
// This function only computes a path — it does not check the file exists;
// callers do that themselves (matching resolve_path.py's docstring, which
// says the same of open.sh's separate `-f` check).
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

	candidate = stripFileURL(strings.TrimSpace(candidate))
	if candidate == "" {
		return "", fmt.Errorf("candidate resolved to empty string")
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
