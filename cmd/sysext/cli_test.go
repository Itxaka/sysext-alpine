package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/overlay"
	"github.com/itxaka/sysext-alpine/internal/release"
)

// NOTE: these tests deliberately exercise only CLI-local code (parsing,
// formatting, refresh skip decision). The internal/* packages are stubs
// under parallel development and their functions panic; nothing here calls
// into them — only their types/constants are used.

func TestClassFromArgv0(t *testing.T) {
	cases := []struct {
		argv0 string
		want  release.Class
	}{
		{"sysext", release.Sysext},
		{"/usr/bin/sysext", release.Sysext},
		{"systemd-sysext", release.Sysext},
		{"confext", release.Confext},
		{"/usr/bin/confext", release.Confext},
		{"systemd-confext", release.Confext},
		{"/some/confext-dir/sysext", release.Sysext}, // only basename counts
	}
	for _, tc := range cases {
		if got := classFromArgv0(tc.argv0); got != tc.want {
			t.Errorf("classFromArgv0(%q) = %v, want %v", tc.argv0, got, tc.want)
		}
	}
}

func TestParseArgsDefaults(t *testing.T) {
	cfg, err := parseArgs([]string{"sysext"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if cfg.class != release.Sysext {
		t.Errorf("class = %v, want Sysext", cfg.class)
	}
	if cfg.verb != "status" {
		t.Errorf("verb = %q, want status", cfg.verb)
	}
	if !cfg.noExec {
		t.Error("noExec default should be true")
	}
	if cfg.jsonMode != jsonOff {
		t.Errorf("jsonMode = %q, want off", cfg.jsonMode)
	}
	if cfg.alwaysRefresh || cfg.force || cfg.noReload || cfg.noLegend ||
		cfg.showHelp || cfg.showVersion {
		t.Error("boolean flags should default to false")
	}
	if cfg.root != "" {
		t.Errorf("root = %q, want empty", cfg.root)
	}
}

func TestParseArgsFlags(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		check func(t *testing.T, cfg *config)
	}{
		{"root equals", []string{"sysext", "--root=/mnt"},
			func(t *testing.T, c *config) {
				if c.root != "/mnt" {
					t.Errorf("root = %q", c.root)
				}
			}},
		{"root separate value", []string{"sysext", "--root", "/mnt"},
			func(t *testing.T, c *config) {
				if c.root != "/mnt" {
					t.Errorf("root = %q", c.root)
				}
			}},
		{"force", []string{"sysext", "--force"},
			func(t *testing.T, c *config) {
				if !c.force {
					t.Error("force not set")
				}
			}},
		{"noexec equals false", []string{"sysext", "--noexec=false"},
			func(t *testing.T, c *config) {
				if c.noExec {
					t.Error("noExec should be false")
				}
			}},
		{"noexec separate no", []string{"sysext", "--noexec", "no"},
			func(t *testing.T, c *config) {
				if c.noExec {
					t.Error("noExec should be false")
				}
			}},
		{"noexec yes", []string{"sysext", "--noexec=yes"},
			func(t *testing.T, c *config) {
				if !c.noExec {
					t.Error("noExec should be true")
				}
			}},
		{"json short", []string{"sysext", "--json=short"},
			func(t *testing.T, c *config) {
				if c.jsonMode != jsonShort {
					t.Errorf("jsonMode = %q", c.jsonMode)
				}
			}},
		{"json pretty separate", []string{"sysext", "--json", "pretty"},
			func(t *testing.T, c *config) {
				if c.jsonMode != jsonPretty {
					t.Errorf("jsonMode = %q", c.jsonMode)
				}
			}},
		{"json off", []string{"sysext", "--json=off"},
			func(t *testing.T, c *config) {
				if c.jsonMode != jsonOff {
					t.Errorf("jsonMode = %q", c.jsonMode)
				}
			}},
		{"no-reload", []string{"sysext", "--no-reload"},
			func(t *testing.T, c *config) {
				if !c.noReload {
					t.Error("noReload not set")
				}
			}},
		{"always-refresh yes", []string{"sysext", "--always-refresh=yes"},
			func(t *testing.T, c *config) {
				if !c.alwaysRefresh {
					t.Error("alwaysRefresh not set")
				}
			}},
		{"always-refresh no", []string{"sysext", "--always-refresh=no"},
			func(t *testing.T, c *config) {
				if c.alwaysRefresh {
					t.Error("alwaysRefresh should be false")
				}
			}},
		{"always-refresh separate", []string{"sysext", "--always-refresh", "yes"},
			func(t *testing.T, c *config) {
				if !c.alwaysRefresh {
					t.Error("alwaysRefresh not set")
				}
			}},
		{"no-pager accepted", []string{"sysext", "--no-pager"},
			func(t *testing.T, c *config) {}},
		{"no-legend", []string{"sysext", "--no-legend"},
			func(t *testing.T, c *config) {
				if !c.noLegend {
					t.Error("noLegend not set")
				}
			}},
		{"confext flag", []string{"sysext", "--confext"},
			func(t *testing.T, c *config) {
				if c.class != release.Confext {
					t.Error("class should be Confext")
				}
			}},
		{"argv0 confext", []string{"/usr/bin/confext", "status"},
			func(t *testing.T, c *config) {
				if c.class != release.Confext {
					t.Error("class should be Confext via argv[0]")
				}
			}},
		{"flags after verb", []string{"sysext", "merge", "--force"},
			func(t *testing.T, c *config) {
				if c.verb != "merge" || !c.force {
					t.Errorf("verb=%q force=%v", c.verb, c.force)
				}
			}},
		{"combined", []string{"sysext", "--root=/x", "--json=short", "--no-legend", "list"},
			func(t *testing.T, c *config) {
				if c.root != "/x" || c.jsonMode != jsonShort || !c.noLegend || c.verb != "list" {
					t.Errorf("got %+v", c)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := parseArgs(tc.args)
			if err != nil {
				t.Fatalf("parseArgs(%v): %v", tc.args, err)
			}
			tc.check(t, cfg)
		})
	}
}

func TestParseArgsVerbs(t *testing.T) {
	for _, verb := range []string{"status", "merge", "unmerge", "refresh", "list"} {
		cfg, err := parseArgs([]string{"sysext", verb})
		if err != nil {
			t.Fatalf("verb %s: %v", verb, err)
		}
		if cfg.verb != verb {
			t.Errorf("verb = %q, want %q", cfg.verb, verb)
		}
	}
}

func TestParseArgsHelpVersion(t *testing.T) {
	for _, args := range [][]string{
		{"sysext", "-h"},
		{"sysext", "--help"},
		{"sysext", "merge", "-h"},
	} {
		cfg, err := parseArgs(args)
		if err != nil {
			t.Fatalf("parseArgs(%v): %v", args, err)
		}
		if !cfg.showHelp {
			t.Errorf("parseArgs(%v): showHelp not set", args)
		}
	}
	cfg, err := parseArgs([]string{"sysext", "--version"})
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if !cfg.showVersion {
		t.Error("showVersion not set")
	}
}

func TestParseArgsErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		errPart string
	}{
		{"unknown long flag", []string{"sysext", "--bogus"}, "unrecognized option '--bogus'"},
		{"unknown short flag", []string{"sysext", "-x"}, "unrecognized option '-x'"},
		{"bool flag with value", []string{"sysext", "--force=1"}, "doesn't allow an argument"},
		{"value flag missing value", []string{"sysext", "--root"}, "requires an argument"},
		{"json missing value", []string{"sysext", "--json"}, "requires an argument"},
		{"bad json mode", []string{"sysext", "--json=banana"}, "unknown JSON output format"},
		{"bad noexec", []string{"sysext", "--noexec=banana"}, "noexec"},
		{"bad always-refresh", []string{"sysext", "--always-refresh=maybe"}, "always-refresh"},
		{"unknown verb", []string{"sysext", "frobnicate"}, "unknown command verb 'frobnicate'"},
		{"too many args", []string{"sysext", "merge", "unmerge"}, "too many arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseArgs(tc.args)
			if err == nil {
				t.Fatalf("parseArgs(%v): expected error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Errorf("error %q does not contain %q", err, tc.errPart)
			}
		})
	}
}

func TestParseBool(t *testing.T) {
	for _, s := range []string{"1", "yes", "y", "true", "t", "on", "YES", "True"} {
		b, err := parseBool(s)
		if err != nil || !b {
			t.Errorf("parseBool(%q) = %v, %v; want true, nil", s, b, err)
		}
	}
	for _, s := range []string{"0", "no", "n", "false", "f", "off", "NO", "False"} {
		b, err := parseBool(s)
		if err != nil || b {
			t.Errorf("parseBool(%q) = %v, %v; want false, nil", s, b, err)
		}
	}
	if _, err := parseBool("maybe"); err == nil {
		t.Error("parseBool(maybe) should fail")
	}
}

func TestRunHelpAndVersion(t *testing.T) {
	var out, errBuf bytes.Buffer
	if err := runWith([]string{"sysext", "--help"}, &out, &errBuf); err != nil {
		t.Fatalf("--help: %v", err)
	}
	for _, want := range []string{"Commands:", "Options:", "merge", "--json=pretty|short|off", "/usr/ and /opt/"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("help output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if err := runWith([]string{"confext", "-h"}, &out, &errBuf); err != nil {
		t.Fatalf("confext -h: %v", err)
	}
	if !strings.Contains(out.String(), "/etc/") {
		t.Errorf("confext help should mention /etc/:\n%s", out.String())
	}

	out.Reset()
	if err := runWith([]string{"sysext", "--version"}, &out, &errBuf); err != nil {
		t.Fatalf("--version: %v", err)
	}
	if !strings.Contains(out.String(), version) {
		t.Errorf("version output %q missing %q", out.String(), version)
	}
}

func TestRunBadFlag(t *testing.T) {
	var out, errBuf bytes.Buffer
	err := runWith([]string{"sysext", "--nope"}, &out, &errBuf)
	if err == nil || !strings.Contains(err.Error(), "unrecognized option") {
		t.Errorf("expected unrecognized option error, got %v", err)
	}
}

func TestFormatTable(t *testing.T) {
	header := []string{"HIERARCHY", "EXTENSIONS", "SINCE"}
	rows := [][]string{
		{"/usr", "foo,bar", "Wed 2026-06-10 10:00:00 UTC"},
		{"/opt", "none", "-"},
	}
	got := formatTable(header, rows, false)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", len(lines), got)
	}
	if !strings.HasPrefix(lines[0], "HIERARCHY ") {
		t.Errorf("header misformatted: %q", lines[0])
	}
	// Columns must align: EXTENSIONS starts at the same offset everywhere.
	idx := strings.Index(lines[0], "EXTENSIONS")
	if strings.Index(lines[1], "foo,bar") != idx || strings.Index(lines[2], "none") != idx {
		t.Errorf("columns not aligned:\n%s", got)
	}
	// Last column unpadded: no trailing spaces.
	for i, l := range lines {
		if strings.TrimRight(l, " ") != l {
			t.Errorf("line %d has trailing spaces: %q", i, l)
		}
	}

	// --no-legend drops the header.
	got = formatTable(header, rows, true)
	if strings.Contains(got, "HIERARCHY") {
		t.Errorf("no-legend output contains header:\n%s", got)
	}
	if !strings.Contains(got, "/usr") {
		t.Errorf("no-legend output missing rows:\n%s", got)
	}

	if formatTable(header, nil, true) != "" {
		t.Error("empty table with no legend should render empty")
	}
}

func TestStatusRows(t *testing.T) {
	statuses := []overlay.Status{
		{Hierarchy: "/usr", Merged: true, Extensions: []string{"foo", "bar"}, Since: 1700000000},
		{Hierarchy: "/opt", Merged: false},
	}
	rows := statusRows(statuses)
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0][0] != "/usr" || rows[0][1] != "foo,bar" {
		t.Errorf("merged row wrong: %v", rows[0])
	}
	if !strings.Contains(rows[0][2], "2023") {
		t.Errorf("SINCE should contain the year: %q", rows[0][2])
	}
	if rows[1][0] != "/opt" || rows[1][1] != "none" || rows[1][2] != "-" {
		t.Errorf("unmerged row wrong: %v", rows[1])
	}
}

func TestStatusJSONShape(t *testing.T) {
	statuses := []overlay.Status{
		{Hierarchy: "/usr", Merged: true, Extensions: []string{"foo"}, Since: 1700000000},
		{Hierarchy: "/opt", Merged: false},
	}
	short, err := renderJSON(statuses, jsonShort)
	if err != nil {
		t.Fatalf("renderJSON short: %v", err)
	}
	if strings.Count(strings.TrimSpace(short), "\n") != 0 {
		t.Errorf("short JSON should be a single line: %q", short)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(short), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("want 2 elements, got %d", len(decoded))
	}
	for _, key := range []string{"hierarchy", "merged", "extensions", "since"} {
		if _, ok := decoded[0][key]; !ok {
			t.Errorf("JSON element missing key %q: %v", key, decoded[0])
		}
	}
	if decoded[0]["hierarchy"] != "/usr" || decoded[0]["merged"] != true {
		t.Errorf("unexpected values: %v", decoded[0])
	}

	pretty, err := renderJSON(statuses, jsonPretty)
	if err != nil {
		t.Fatalf("renderJSON pretty: %v", err)
	}
	if !strings.Contains(pretty, "\n  ") {
		t.Errorf("pretty JSON should be indented: %q", pretty)
	}
	var decodedPretty []map[string]any
	if err := json.Unmarshal([]byte(pretty), &decodedPretty); err != nil {
		t.Fatalf("pretty JSON invalid: %v", err)
	}
}

func TestListEntriesJSONShape(t *testing.T) {
	images := []discover.Image{
		{Name: "foo", Path: "/var/lib/extensions/foo.raw", Type: discover.TypeRaw, ModTime: 1700000000},
		{Name: "bar", Path: "/etc/extensions/bar", Type: discover.TypeDirectory, ModTime: 1700000001},
	}
	entries := toListEntries(images)
	out, err := renderJSON(entries, jsonShort)
	if err != nil {
		t.Fatalf("renderJSON: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(decoded) != 2 {
		t.Fatalf("want 2 elements, got %d", len(decoded))
	}
	for _, key := range []string{"name", "type", "path", "mtime"} {
		if _, ok := decoded[0][key]; !ok {
			t.Errorf("missing key %q in %v", key, decoded[0])
		}
	}
	if decoded[0]["type"] != "raw" || decoded[1]["type"] != "directory" {
		t.Errorf("types wrong: %v / %v", decoded[0]["type"], decoded[1]["type"])
	}

	// Empty list must encode as [], not null.
	out, err = renderJSON(toListEntries(nil), jsonShort)
	if err != nil {
		t.Fatalf("renderJSON empty: %v", err)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty list JSON = %q, want []", out)
	}
}

func TestListRows(t *testing.T) {
	entries := toListEntries([]discover.Image{
		{Name: "foo", Path: "/p/foo.raw", Type: discover.TypeRaw, ModTime: 1700000000},
		{Name: "zero", Path: "/p/zero", Type: discover.TypeDirectory, ModTime: 0},
	})
	rows := listRows(entries)
	if rows[0][0] != "foo" || rows[0][1] != "raw" || rows[0][2] != "/p/foo.raw" {
		t.Errorf("row wrong: %v", rows[0])
	}
	if !strings.Contains(rows[0][3], "2023") {
		t.Errorf("TIME should contain year: %q", rows[0][3])
	}
	if rows[1][3] != "-" {
		t.Errorf("zero mtime should render '-': %q", rows[1][3])
	}
}

func TestImageTypeString(t *testing.T) {
	if got := imageTypeString(discover.TypeDirectory); got != "directory" {
		t.Errorf("TypeDirectory = %q", got)
	}
	if got := imageTypeString(discover.TypeRaw); got != "raw" {
		t.Errorf("TypeRaw = %q", got)
	}
	if got := imageTypeString(discover.ImageType(99)); got != "unknown" {
		t.Errorf("bogus type = %q", got)
	}
}

func TestShouldSkipRefresh(t *testing.T) {
	cases := []struct {
		name       string
		discovered []string
		mergedSets [][]string
		always     bool
		want       bool
	}{
		{"identical single hierarchy", []string{"a", "b"},
			[][]string{{"a", "b"}, nil}, false, true},
		{"identical all hierarchies", []string{"a", "b"},
			[][]string{{"a", "b"}, {"a", "b"}}, false, true},
		{"always-refresh forces", []string{"a", "b"},
			[][]string{{"a", "b"}}, true, false},
		{"nothing merged", []string{"a", "b"},
			[][]string{nil, nil}, false, false},
		{"different set", []string{"a", "b"},
			[][]string{{"a"}}, false, false},
		{"different order", []string{"a", "b"},
			[][]string{{"b", "a"}}, false, false},
		{"extra merged extension", []string{"a"},
			[][]string{{"a", "b"}}, false, false},
		{"one hierarchy stale", []string{"a", "b"},
			[][]string{{"a", "b"}, {"a"}}, false, false},
		{"no hierarchies", []string{"a"}, nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldSkipRefresh(tc.discovered, tc.mergedSets, tc.always)
			if got != tc.want {
				t.Errorf("shouldSkipRefresh(%v, %v, %v) = %v, want %v",
					tc.discovered, tc.mergedSets, tc.always, got, tc.want)
			}
		})
	}
}

func TestFormatTimestamp(t *testing.T) {
	got := formatTimestamp(1700000000) // 2023-11-14/15 depending on zone
	if !strings.Contains(got, "2023") {
		t.Errorf("formatTimestamp = %q, want year 2023", got)
	}
}
