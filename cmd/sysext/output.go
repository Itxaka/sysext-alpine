// Output helpers: column-aligned tables (systemd table style) and
// --json=short|pretty rendering.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/overlay"
)

// formatTable renders rows as a left-aligned, space-padded table. The header
// is omitted when noLegend is set. The last column is never padded.
func formatTable(header []string, rows [][]string, noLegend bool) string {
	var all [][]string
	if !noLegend {
		all = append(all, header)
	}
	all = append(all, rows...)
	if len(all) == 0 {
		return ""
	}

	widths := make([]int, len(all[0]))
	for _, row := range all {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	var b strings.Builder
	for _, row := range all {
		for i, cell := range row {
			if i == len(row)-1 {
				b.WriteString(cell)
			} else {
				fmt.Fprintf(&b, "%-*s ", widths[i], cell)
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// renderJSON marshals v per --json mode (jsonShort = compact, jsonPretty =
// indented), with a trailing newline.
func renderJSON(v any, mode string) (string, error) {
	var (
		b   []byte
		err error
	)
	switch mode {
	case jsonPretty:
		b, err = json.MarshalIndent(v, "", "  ")
	default: // jsonShort
		b, err = json.Marshal(v)
	}
	if err != nil {
		return "", fmt.Errorf("failed to format JSON output: %w", err)
	}
	return string(b) + "\n", nil
}

// formatTimestamp renders a unix timestamp in systemd's local-time style,
// e.g. "Wed 2026-06-10 18:04:05 CEST".
func formatTimestamp(unix int64) string {
	return time.Unix(unix, 0).Local().Format("Mon 2006-01-02 15:04:05 MST")
}

// statusRows converts hierarchy statuses to table rows
// (HIERARCHY / EXTENSIONS / SINCE).
func statusRows(statuses []overlay.Status) [][]string {
	rows := make([][]string, 0, len(statuses))
	for _, s := range statuses {
		extensions, since := "none", "-"
		if s.Merged {
			if len(s.Extensions) > 0 {
				extensions = strings.Join(s.Extensions, ",")
			}
			if s.Since != 0 {
				since = formatTimestamp(s.Since)
			}
		}
		rows = append(rows, []string{s.Hierarchy, extensions, since})
	}
	return rows
}

// listEntry is the JSON shape of one `list` line.
type listEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Path  string `json:"path"`
	MTime int64  `json:"mtime"` // unix seconds
}

// imageTypeString maps discover.ImageType to display text. Implemented
// locally (instead of ImageType.String) so output stays decoupled from the
// discover package's stringer.
func imageTypeString(t discover.ImageType) string {
	switch t {
	case discover.TypeDirectory:
		return "directory"
	case discover.TypeRaw:
		return "raw"
	}
	return "unknown"
}

// toListEntries converts discovered images for `list` output. Always returns
// a non-nil slice so JSON output is [] rather than null.
func toListEntries(images []discover.Image) []listEntry {
	entries := make([]listEntry, 0, len(images))
	for _, img := range images {
		entries = append(entries, listEntry{
			Name:  img.Name,
			Type:  imageTypeString(img.Type),
			Path:  img.Path,
			MTime: img.ModTime,
		})
	}
	return entries
}

// listRows converts list entries to table rows (NAME / TYPE / PATH / TIME).
func listRows(entries []listEntry) [][]string {
	rows := make([][]string, 0, len(entries))
	for _, e := range entries {
		t := "-"
		if e.MTime != 0 {
			t = formatTimestamp(e.MTime)
		}
		rows = append(rows, []string{e.Name, e.Type, e.Path, t})
	}
	return rows
}
