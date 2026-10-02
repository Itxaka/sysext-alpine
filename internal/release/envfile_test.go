package release

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    Fields
	}{
		{"plain assignment", "ID=alpine\nVERSION_ID=3.20\n",
			Fields{"ID": "alpine", "VERSION_ID": "3.20"}},
		{"double quoted", `NAME="Alpine Linux"`, Fields{"NAME": "Alpine Linux"}},
		{"single quoted", `PRETTY_NAME='Alpine Linux v3.20'`, Fields{"PRETTY_NAME": "Alpine Linux v3.20"}},
		{"empty values", "a=\nb=\"\"\nc=''\nd=", Fields{"a": "", "b": "", "c": "", "d": ""}},
		{"last assignment wins", "ID=a\nID=b\n", Fields{"ID": "b"}},
		{"crlf", "ID=alpine\r\nVERSION_ID=3.20\r\n", Fields{"ID": "alpine", "VERSION_ID": "3.20"}},
		{"carriage return only", "A=1\rB=2", Fields{"A": "1", "B": "2"}},
		{"no trailing newline", "ID=alpine", Fields{"ID": "alpine"}},
		{"empty", "", Fields{}},

		// whitespace around keys and '='
		{"spaces around equals", "ID = alpine\n", Fields{"ID": "alpine"}},
		{"leading whitespace", "  \tID=alpine\n", Fields{"ID": "alpine"}},
		{"trailing value whitespace chomped", "ID=alpine  \t\n", Fields{"ID": "alpine"}},
		{"inner value whitespace kept", "A=a  b\n", Fields{"A": "a  b"}},
		{"key with inner space", "export nine=nineval\n", Fields{"export nine": "nineval"}},

		// comments
		{"hash comment", "# ID=fedora\nID=alpine\n", Fields{"ID": "alpine"}},
		{"semicolon comment", ";ID=fedora\nID=alpine\n", Fields{"ID": "alpine"}},
		{"indented comment", "  # x\n ; y\nID=alpine\n", Fields{"ID": "alpine"}},
		{"hash inside value is data", "A=b #c\n", Fields{"A": "b #c"}},
		{"comment continuation is ignored", "#x \\\nA=1\n", Fields{"A": "1"}},

		// escapes
		{"unquoted escape", `V=a\ b`, Fields{"V": "a b"}},
		{"unquoted backslash dropped", `eleven=\value`, Fields{"eleven": "value"}},
		{"line continuation", "ID=roll\\\ning\n", Fields{"ID": "rolling"}},
		{"continuation in version", "A=1.\\\n0\n", Fields{"A": "1.0"}},
		{"escaped trailing space kept", "g=g\\ \n", Fields{"g": "g "}},
		{"double quote escapes", `A="a\"b\\c\$d` + "\\`" + `e\nf"`, Fields{"A": "a\"b\\c$d`e\\nf"}},
		{"double quote continuation", "A=\"a\\\nb\"", Fields{"A": "ab"}},
		{"single quote keeps backslashes", `thirteen='\value'`, Fields{"thirteen": `\value`}},
		{"double quote keeps unknown escape", `twelve="\value"`, Fields{"twelve": `\value`}},

		// quotes
		{"quoted value spans lines", "X=\"a\nID=evil\"\n", Fields{"X": "a\nID=evil"}},
		{"concatenation", `A="x"'y'z`, Fields{"A": "xyz"}},
		{"concatenation with spaces", `five = "55\"55" "FIVE" cinco   `, Fields{"five": `55"55FIVEcinco`}},
		{"unterminated double quote keeps value", `A="abc`, Fields{"A": "abc"}},
		{"unterminated single quote keeps value", "A='abc\nB=1", Fields{"A": "abc\nB=1"}},
		{"trailing text after quote appended", `A="x" y`, Fields{"A": "xy"}},
		{"comment after quote is data", `seven="sevenval" #nocomment`, Fields{"seven": "sevenval#nocomment"}},

		// malformed lines
		{"line without equals ignored", "garbage\nID=alpine\n", Fields{"ID": "alpine"}},
		{"key starting with equals", "==x\n", Fields{"=": "x"}},
		{"NUL ends input", "ID=alpine\x00VERSION_ID=1\n", Fields{"ID": "alpine"}},
		{"key without value at EOF ignored", "ID", Fields{}},
		{"dangling escape at EOF", `A=x\`, Fields{"A": "x"}},

		// systemd test-env-file.c load_env_file_1..6
		{"load_env_file_1",
			"a=a\na=b\na=b\na=a\nb=b\\\nc\nd= d\\\ne  \\\nf  \ng=g\\ \nh= ąęół\\ śćńźżμ \ni=i\\",
			Fields{"a": "a", "b": "bc", "d": "de  f", "g": "g ", "h": "ąęół śćńźżμ", "i": "i"}},
		{"load_env_file_2", "a=a\\\n", Fields{"a": "a"}},
		{"load_env_file_3",
			"#SPAMD_ARGS=\"-d --socketpath=/var/lib/bulwark/spamd \\\n" +
				"#--nouser-config                                     \\\n" +
				"normal1=line\\\n" +
				"111\n" +
				";normal=ignored                                      \\\n" +
				"normal2=line222\n" +
				"normal ignored                                       \\\n",
			Fields{"normal1": "line111", "normal2": "line222"}},
		{"load_env_file_4",
			"# Generated\n\nHWMON_MODULES=\"coretemp f71882fg\"\n\n# For compatibility reasons\n\nMODULE_0=coretemp\nMODULE_1=f71882fg",
			Fields{"HWMON_MODULES": "coretemp f71882fg", "MODULE_0": "coretemp", "MODULE_1": "f71882fg"}},
		{"load_env_file_5", "a=\nb=", Fields{"a": "", "b": ""}},
		{"load_env_file_6",
			"a=\\ \\n \\t \\x \\y \\' \nb= \\$'                  \nc= ' \\n\\t\\$\\`\\\\\n'   \nd= \" \\n\\t\\$\\`\\\\\n\"   \n",
			Fields{"a": " n t x y '", "b": "$'", "c": " \\n\\t\\$\\`\\\\\n", "d": " \\n\\t$`\\\n"}},
		{"parse_env_file",
			"one=BAR   \n# comment\n # comment \n ; comment \n  two   =   bar    \ninvalid line\ninvalid line #comment\n" +
				"three = \"333\nxxxx\"\nfour = '44\\\"44'\nfive = \"55\\\"55\" \"FIVE\" cinco   \nsix = seis sechs\\\n sis\n" +
				"seven=\"sevenval\" #nocomment\neight=eightval #nocomment\nexport nine=nineval\nten=ignored\nten=ignored\nten=\n" +
				"eleven=\\value\ntwelve=\"\\value\"\nthirteen='\\value'",
			Fields{"one": "BAR", "two": "bar", "three": "333\nxxxx", "four": `44\"44`, "five": `55"55FIVEcinco`,
				"six": "seis sechs sis", "seven": "sevenval#nocomment", "eight": "eightval #nocomment",
				"export nine": "nineval", "ten": "", "eleven": "value", "twelve": `\value`, "thirteen": `\value`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse([]byte(tc.content))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if !maps.Equal(got, tc.want) {
				t.Errorf("Parse() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseInvalidUTF8(t *testing.T) {
	for _, s := range []string{
		"fo\ufffeo=bar",
		"foo=b\uffffar",
		"baz=hello world\ufffe",
		"ID=\xff\n",
		"\xc3=x\n",
		"A=\ufdd0\n",
		"A=\xed\xa0\x80\n",
	} {
		if got, err := Parse([]byte(s)); err == nil {
			t.Errorf("Parse(%q) = %q, want error", s, got)
		}
	}
	if _, err := Parse([]byte("A=\ufffd\n")); err != nil {
		t.Errorf("U+FFFD is valid UTF-8: %v", err)
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "os-release")
	mustWriteFile(t, p, "ID=alpine\n")
	got, err := ParseFile(p)
	if err != nil || got["ID"] != "alpine" {
		t.Errorf("ParseFile() = %v, %v", got, err)
	}
	if _, err := ParseFile(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want ErrNotExist", err)
	}
	if _, err := ParseFile(dir); err == nil {
		t.Error("directory: want error")
	}
	mustSymlink(t, p, filepath.Join(dir, "link"))
	if _, err := ParseFile(filepath.Join(dir, "link")); err == nil {
		t.Error("ParseFile follows a final symlink; callers must resolve paths first")
	}
}

func TestParseBoolean(t *testing.T) {
	for _, v := range []string{"1", "yes", "y", "true", "t", "on", "YES", "True", "On", "Y"} {
		if b, err := ParseBoolean(v); err != nil || !b {
			t.Errorf("ParseBoolean(%q) = %v, %v; want true", v, b, err)
		}
	}
	for _, v := range []string{"0", "no", "n", "false", "f", "off", "NO", "False", "OFF", "N", "F"} {
		if b, err := ParseBoolean(v); err != nil || b {
			t.Errorf("ParseBoolean(%q) = %v, %v; want false", v, b, err)
		}
	}
	for _, v := range []string{"", " 1", "0 ", "maybe", "2", "yess", "\u0130"} {
		if _, err := ParseBoolean(v); err == nil {
			t.Errorf("ParseBoolean(%q): want error", v)
		}
	}
}

// serialize writes fields so that Parse reads them back unchanged.
func serialize(f Fields) []byte {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	for _, k := range keys {
		b.WriteString(k + `="` + r.Replace(f[k]) + "\"\n")
	}
	return []byte(b.String())
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"ID=alpine\nVERSION_ID=3.20\n",
		`A="x"'y'z` + "\nB=a\\ b\n# c\n;d\n",
		"X=\"a\nID=evil\"\n",
		"==x\nexport nine=nineval\n",
		"a=\\ \\n \\t \\x \\y \\' \nb= \\$'\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Parse(data)
		if err != nil {
			return
		}
		again, err := Parse(serialize(got))
		if err != nil {
			t.Fatalf("re-parse of %q failed: %v", serialize(got), err)
		}
		if !maps.Equal(got, again) {
			t.Fatalf("not idempotent: %q -> %q", got, again)
		}
	})
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	mustMkdirAll(t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
