package main

import "testing"

func TestParseOpenArgs(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantOpts openOpts
		wantErr  bool
	}{
		{
			name:     "no args",
			args:     nil,
			wantOpts: openOpts{},
		},
		{
			name:     "path only",
			args:     []string{"/tmp/a.pdf"},
			wantOpts: openOpts{path: "/tmp/a.pdf"},
		},
		{
			name:     "fallback then path",
			args:     []string{"--fallback=system", "/tmp/a.txt"},
			wantOpts: openOpts{fallback: "system", path: "/tmp/a.txt"},
		},
		{
			name:     "path then fallback",
			args:     []string{"/tmp/a.txt", "--fallback=system"},
			wantOpts: openOpts{fallback: "system", path: "/tmp/a.txt"},
		},
		{
			name:     "fallback none is a no-op",
			args:     []string{"--fallback=none", "/tmp/a.pdf"},
			wantOpts: openOpts{fallback: "", path: "/tmp/a.pdf"},
		},
		{
			name:    "unknown flag",
			args:    []string{"--bogus", "/tmp/a.pdf"},
			wantErr: true,
		},
		{
			name:    "unknown fallback value",
			args:    []string{"--fallback=bogus", "/tmp/a.pdf"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOpenArgs(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseOpenArgs(%v) = %+v, nil; want error", tc.args, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseOpenArgs(%v) unexpected error: %v", tc.args, err)
			}
			if got != tc.wantOpts {
				t.Fatalf("parseOpenArgs(%v) = %+v, want %+v", tc.args, got, tc.wantOpts)
			}
		})
	}
}

func TestSystemOpenerArgv(t *testing.T) {
	cases := []struct {
		goos     string
		path     string
		wantArgv []string
		wantErr  bool
	}{
		{goos: "darwin", path: "/tmp/a.txt", wantArgv: []string{"open", "/tmp/a.txt"}},
		{goos: "linux", path: "/tmp/a.txt", wantArgv: []string{"xdg-open", "/tmp/a.txt"}},
		{goos: "windows", path: "/tmp/a.txt", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			got, err := systemOpenerArgv(tc.goos, tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("systemOpenerArgv(%q, %q) = %v, nil; want error", tc.goos, tc.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("systemOpenerArgv(%q, %q) unexpected error: %v", tc.goos, tc.path, err)
			}
			if len(got) != len(tc.wantArgv) {
				t.Fatalf("systemOpenerArgv(%q, %q) = %v, want %v", tc.goos, tc.path, got, tc.wantArgv)
			}
			for i := range got {
				if got[i] != tc.wantArgv[i] {
					t.Fatalf("systemOpenerArgv(%q, %q) = %v, want %v", tc.goos, tc.path, got, tc.wantArgv)
				}
			}
		})
	}
}
