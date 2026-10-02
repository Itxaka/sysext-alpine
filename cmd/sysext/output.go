package main

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/overlay"
)

// formatTable renders rows like systemd's table_print(): every column but the
// last is padded to its width and followed by one space. Cells may span
// several lines ("\n"); the other columns are blank on continuation lines.
// The header is omitted when legend is false.
func formatTable(header []string, rows [][]string, legend bool) string {
	var all [][][]string
	if legend {
		all = append(all, splitCells(header))
	}
	for _, r := range rows {
		all = append(all, splitCells(r))
	}
	widths := make([]int, len(header))
	for _, row := range all {
		for i, cell := range row {
			for _, line := range cell {
				widths[i] = max(widths[i], len([]rune(line)))
			}
		}
	}
	var b strings.Builder
	for _, row := range all {
		height := 1
		for _, cell := range row {
			height = max(height, len(cell))
		}
		for l := range height {
			for i, cell := range row {
				text := ""
				if l < len(cell) {
					text = cell[l]
				}
				if i == len(row)-1 {
					b.WriteString(text)
					break
				}
				b.WriteString(text)
				b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(text))+1))
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func splitCells(row []string) [][]string {
	cells := make([][]string, len(row))
	for i, c := range row {
		cells[i] = strings.Split(c, "\n")
	}
	return cells
}

// jsonObject is a JSON object with ordered keys.
type jsonObject []jsonField

type jsonField struct {
	key   string
	value any
}

// formatJSON renders v like sd_json_variant_dump() with
// SD_JSON_FORMAT_NEWLINE, plus SD_JSON_FORMAT_PRETTY when pretty is set. v
// is built from nil, bool, string, int64, []string, []any and jsonObject.
func formatJSON(v any, pretty bool) string {
	var b strings.Builder
	writeJSON(&b, v, pretty, "")
	b.WriteByte('\n')
	return b.String()
}

func writeJSON(b *strings.Builder, v any, pretty bool, prefix string) {
	switch v := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case int64:
		b.WriteString(strconv.FormatInt(v, 10))
	case string:
		writeJSONString(b, v)
	case []string:
		a := make([]any, len(v))
		for i, s := range v {
			a[i] = s
		}
		writeJSON(b, a, pretty, prefix)
	case []any:
		writeJSONList(b, '[', ']', len(v), pretty, prefix, func(i int, inner string) {
			writeJSON(b, v[i], pretty, inner)
		})
	case jsonObject:
		writeJSONList(b, '{', '}', len(v), pretty, prefix, func(i int, inner string) {
			writeJSONString(b, v[i].key)
			if pretty {
				b.WriteString(" : ")
			} else {
				b.WriteByte(':')
			}
			writeJSON(b, v[i].value, pretty, inner)
		})
	default:
		panic(fmt.Sprintf("formatJSON: unsupported type %T", v))
	}
}

func writeJSONList(b *strings.Builder, open, closing byte, n int, pretty bool, prefix string, elem func(i int, prefix string)) {
	if n == 0 {
		b.WriteByte(open)
		b.WriteByte(closing)
		return
	}
	inner := prefix
	b.WriteByte(open)
	if pretty {
		inner += "\t"
		b.WriteByte('\n')
	}
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
			if pretty {
				b.WriteByte('\n')
			}
		}
		if pretty {
			b.WriteString(inner)
		}
		elem(i, inner)
	}
	if pretty {
		b.WriteByte('\n')
		b.WriteString(prefix)
	}
	b.WriteByte(closing)
}

// writeJSONString is json_format_string(): only quotes, backslashes and
// control characters are escaped, everything else is written as is.
func writeJSONString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := range len(s) {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < ' ' {
				fmt.Fprintf(b, `\u%04x`, c)
			} else {
				b.WriteByte(c)
			}
		}
	}
	b.WriteByte('"')
}

// formatTimestamp renders a time in microseconds like systemd's
// format_timestamp(), e.g. "Wed 2026-06-10 18:04:05 CEST"; 0 renders "-".
func formatTimestamp(usec int64) string {
	if usec <= 0 {
		return "-"
	}
	return time.UnixMicro(usec).Local().Format("Mon 2006-01-02 15:04:05 MST")
}

// comparePaths is systemd's path_compare(): absolute paths sort before
// relative ones, then component by component.
func comparePaths(a, b string) int {
	if aa, ba := strings.HasPrefix(a, "/"), strings.HasPrefix(b, "/"); aa != ba {
		if aa {
			return -1
		}
		return 1
	}
	return slices.Compare(pathComponents(a), pathComponents(b))
}

func pathComponents(p string) []string {
	var out []string
	for c := range strings.SplitSeq(p, "/") {
		if c != "" && c != "." {
			out = append(out, c)
		}
	}
	return out
}

// statusOutput renders `status` in the format of systemd-sysext 262: the
// table shows "-" for empty cells and one extension per line, the JSON
// output an "extensions" array and "since" in microseconds or null.
func statusOutput(statuses []overlay.Status, jsonMode string, legend bool) string {
	statuses = slices.Clone(statuses)
	slices.SortStableFunc(statuses, func(a, b overlay.Status) int { return comparePaths(a.Hierarchy, b.Hierarchy) })
	if jsonMode != jsonOff {
		list := make([]any, 0, len(statuses))
		for _, s := range statuses {
			var since any
			if s.Merged {
				since = s.Since
			}
			list = append(list, jsonObject{
				{"hierarchy", s.Hierarchy},
				{"extensions", append([]string{}, s.Extensions...)},
				{"since", since},
			})
		}
		return formatJSON(list, jsonMode == jsonPretty)
	}
	rows := make([][]string, 0, len(statuses))
	for _, s := range statuses {
		exts, since := "-", "-"
		if len(s.Extensions) > 0 {
			exts = strings.Join(s.Extensions, "\n")
		}
		if s.Merged {
			since = formatTimestamp(s.Since)
		}
		rows = append(rows, []string{s.Hierarchy, exts, since})
	}
	return formatTable([]string{"HIERARCHY", "EXTENSIONS", "SINCE"}, rows, legend)
}

// listOutput renders `list` like systemd-sysext 262: sorted by name, the
// time is the modification time, or the birth time for directories.
func listOutput(images []discover.Image, jsonMode string, legend bool) string {
	images = slices.Clone(images)
	slices.SortStableFunc(images, func(a, b discover.Image) int { return strings.Compare(a.Name, b.Name) })
	if jsonMode != jsonOff {
		list := make([]any, 0, len(images))
		for _, img := range images {
			list = append(list, jsonObject{
				{"name", img.Name},
				{"type", img.Type.String()},
				{"path", img.Path},
				{"time", img.Time()},
			})
		}
		return formatJSON(list, jsonMode == jsonPretty)
	}
	rows := make([][]string, 0, len(images))
	for _, img := range images {
		rows = append(rows, []string{img.Name, img.Type.String(), img.Path, formatTimestamp(img.Time())})
	}
	return formatTable([]string{"NAME", "TYPE", "PATH", "TIME"}, rows, legend)
}

// columns is systemd's columns(): $COLUMNS, else the width of the terminal
// on stdout, else 80.
func columns() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n
	}
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
		return int(ws.Col)
	}
	return 80
}

// wrapWords breaks s at spaces into lines of at most width characters; a
// single longer word gets a line of its own.
func wrapWords(s string, width int) []string {
	var lines []string
	line := ""
	for w := range strings.FieldsSeq(s) {
		switch {
		case line == "":
			line = w
		case len(line)+1+len(w) <= width:
			line += " " + w
		default:
			lines = append(lines, line)
			line = w
		}
	}
	return append(lines, line)
}
