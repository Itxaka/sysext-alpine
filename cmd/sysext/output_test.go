package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/itxaka/sysext-alpine/internal/discover"
	"github.com/itxaka/sysext-alpine/internal/overlay"
)

func TestFormatTable(t *testing.T) {
	header := []string{"HIERARCHY", "EXTENSIONS", "SINCE"}
	rows := [][]string{
		{"/opt", "aaa\nbbb\nccc", "Fri 2026-09-25 14:40:31 CEST"},
		{"/usr", "-", "-"},
	}
	want := "HIERARCHY EXTENSIONS SINCE\n" +
		"/opt      aaa        Fri 2026-09-25 14:40:31 CEST\n" +
		"          bbb        \n" +
		"          ccc        \n" +
		"/usr      -          -\n"
	if got := formatTable(header, rows, true); got != want {
		t.Errorf("table:\n%q\nwant\n%q", got, want)
	}
	want = "/opt aaa Fri 2026-09-25 14:40:31 CEST\n" +
		"     bbb \n" +
		"     ccc \n" +
		"/usr -   -\n"
	if got := formatTable(header, rows, false); got != want {
		t.Errorf("no legend:\n%q\nwant\n%q", got, want)
	}
	if got := formatTable(header, nil, true); got != "HIERARCHY EXTENSIONS SINCE\n" {
		t.Errorf("empty table = %q", got)
	}
	if got := formatTable(header, nil, false); got != "" {
		t.Errorf("empty table without legend = %q", got)
	}
}

func TestFormatJSON(t *testing.T) {
	v := []any{
		jsonObject{{"a", "x&y<z> \"\\\b\f\n\r\t\x01\x7f"}, {"b", []string{}}, {"c", nil}},
		jsonObject{{"d", []string{"p", "q"}}, {"e", int64(-5)}, {"f", true}, {"g", jsonObject{}}},
	}
	short := `[{"a":"x&y<z>` + " " + `\"\\\b\f\n\r\t\u0001` + "\x7f" + `","b":[],"c":null},{"d":["p","q"],"e":-5,"f":true,"g":{}}]` + "\n"
	if got := formatJSON(v, false); got != short {
		t.Errorf("short:\n%q\nwant\n%q", got, short)
	}
	pretty := "[\n" +
		"\t{\n" +
		"\t\t\"a\" : \"x&y<z> \\\"\\\\\\b\\f\\n\\r\\t\\u0001\x7f\",\n" +
		"\t\t\"b\" : [],\n" +
		"\t\t\"c\" : null\n" +
		"\t},\n" +
		"\t{\n" +
		"\t\t\"d\" : [\n" +
		"\t\t\t\"p\",\n" +
		"\t\t\t\"q\"\n" +
		"\t\t],\n" +
		"\t\t\"e\" : -5,\n" +
		"\t\t\"f\" : true,\n" +
		"\t\t\"g\" : {}\n" +
		"\t}\n" +
		"]\n"
	if got := formatJSON(v, true); got != pretty {
		t.Errorf("pretty:\n%s\nwant\n%s", got, pretty)
	}
	if got := formatJSON([]any{}, true); got != "[]\n" {
		t.Errorf("empty = %q", got)
	}
	var decoded []map[string]any
	if err := json.Unmarshal([]byte(pretty), &decoded); err != nil {
		t.Errorf("pretty output is not JSON: %v", err)
	}
}

func TestStatusOutput(t *testing.T) {
	unmerged := []overlay.Status{{Hierarchy: "/usr"}, {Hierarchy: "/opt"}}
	if got, want := statusOutput(unmerged, jsonShort, true),
		`[{"hierarchy":"/opt","extensions":[],"since":null},{"hierarchy":"/usr","extensions":[],"since":null}]`+"\n"; got != want {
		t.Errorf("unmerged short:\n%s\nwant\n%s", got, want)
	}
	wantPretty := "[\n\t{\n\t\t\"hierarchy\" : \"/opt\",\n\t\t\"extensions\" : [],\n\t\t\"since\" : null\n\t},\n" +
		"\t{\n\t\t\"hierarchy\" : \"/usr\",\n\t\t\"extensions\" : [],\n\t\t\"since\" : null\n\t}\n]\n"
	if got := statusOutput(unmerged, jsonPretty, true); got != wantPretty {
		t.Errorf("unmerged pretty:\n%s\nwant\n%s", got, wantPretty)
	}
	if got, want := statusOutput(unmerged, jsonOff, true), "HIERARCHY EXTENSIONS SINCE\n/opt      -          -\n/usr      -          -\n"; got != want {
		t.Errorf("unmerged table:\n%s\nwant\n%s", got, want)
	}

	since := int64(1700000000123456)
	merged := []overlay.Status{
		{Hierarchy: "/usr", Merged: true, Extensions: []string{"foo", "bar"}, Since: since},
		{Hierarchy: "/etc", Merged: true, Since: since},
	}
	if got, want := statusOutput(merged, jsonShort, true),
		`[{"hierarchy":"/etc","extensions":[],"since":1700000000123456},{"hierarchy":"/usr","extensions":["foo","bar"],"since":1700000000123456}]`+"\n"; got != want {
		t.Errorf("merged short:\n%s\nwant\n%s", got, want)
	}
	table := statusOutput(merged, jsonOff, false)
	ts := formatTimestamp(since)
	if want := "/etc -   " + ts + "\n/usr foo " + ts + "\n     bar \n"; table != want {
		t.Errorf("merged table:\n%q\nwant\n%q", table, want)
	}
}

func TestComparePaths(t *testing.T) {
	paths := []string{"/usr", "/usr-x", "/opt", "/usr/x", "rel", "/a//b", "/"}
	slices.SortFunc(paths, comparePaths)
	if want := []string{"/", "/a//b", "/opt", "/usr", "/usr/x", "/usr-x", "rel"}; !slices.Equal(paths, want) {
		t.Errorf("sorted = %q, want %q", paths, want)
	}
	if comparePaths("/usr/", "/usr") != 0 {
		t.Error("trailing slashes do not matter")
	}
}

func TestListOutput(t *testing.T) {
	images := []discover.Image{
		{Name: "foo-9", Path: "/etc/extensions/foo-9", Type: discover.TypeDirectory, CrTime: 1700000000654321},
		{Name: "foo-10", Path: "/var/lib/extensions/foo-10.raw", Type: discover.TypeRaw, MTime: 1700000000123456, CrTime: 5},
		{Name: "x&y", Path: "/dev/x&y", Type: discover.TypeBlock},
	}
	want := `[{"name":"foo-10","type":"raw","path":"/var/lib/extensions/foo-10.raw","time":1700000000123456},` +
		`{"name":"foo-9","type":"directory","path":"/etc/extensions/foo-9","time":1700000000654321},` +
		`{"name":"x&y","type":"block","path":"/dev/x&y","time":0}]` + "\n"
	if got := listOutput(images, jsonShort, true); got != want {
		t.Errorf("list json:\n%s\nwant\n%s", got, want)
	}
	lines := strings.Split(listOutput(images, jsonOff, true), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "NAME   TYPE      PATH") || !strings.HasPrefix(lines[1], "foo-10 raw") ||
		!strings.HasSuffix(lines[2], formatTimestamp(1700000000654321)) || !strings.HasSuffix(lines[3], " -") {
		t.Errorf("list table:\n%s", strings.Join(lines, "\n"))
	}
	if images[0].Name != "foo-9" {
		t.Error("listOutput must not reorder its argument")
	}
}

func TestFormatTimestamp(t *testing.T) {
	if got := formatTimestamp(0); got != "-" {
		t.Errorf("zero = %q", got)
	}
	ts := time.Date(2026, 6, 10, 18, 4, 5, 0, time.Local)
	if got, want := formatTimestamp(ts.UnixMicro()), ts.Format("Mon 2006-01-02 15:04:05 MST"); got != want || !strings.HasPrefix(got, "Wed 2026-06-10 18:04:05 ") {
		t.Errorf("formatTimestamp = %q, want %q", got, want)
	}
}

func TestWrapWords(t *testing.T) {
	got := wrapWords("Specify a mutability mode (yes, no, auto, import, ephemeral, ephemeral-import, help)", 53)
	want := []string{"Specify a mutability mode (yes, no, auto, import,", "ephemeral, ephemeral-import, help)"}
	if !slices.Equal(got, want) {
		t.Errorf("wrapWords = %q", got)
	}
	if got := wrapWords("averyveryverylongword x", 5); !slices.Equal(got, []string{"averyveryverylongword", "x"}) {
		t.Errorf("long word = %q", got)
	}
}
