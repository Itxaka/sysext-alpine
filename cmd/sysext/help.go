package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/itxaka/sysext-alpine/internal/release"
)

// abstract is the one-line description of the class' command.
func abstract(class release.Class) string {
	if class == release.Confext {
		return "Merge configuration extension images into /etc/."
	}
	return "Merge system extension images into /usr/ and /opt/."
}

// printHelp prints the help in the layout of systemd 262's
// command_print_help(): the synopsis, the abstract, the verb and option
// tables sharing one column width, and the man page reference.
func (c *cli) printHelp() {
	prog := c.cfg.progName
	if v := filepath.Base(os.Getenv("SYSTEMD_INVOKED_AS")); v != "." && v != "/" {
		prog = v
	}
	var cmds, opts [][2]string
	for i, v := range verbs {
		if v.help == "" {
			continue
		}
		name := v.name
		if i == 0 {
			name = "[" + name + "]"
		}
		cmds = append(cmds, [2]string{"  " + name, v.help})
	}
	for _, o := range options {
		if o.help == "" {
			continue
		}
		syn := "     --" + o.long
		if o.short != 0 {
			syn = "  -" + string(o.short) + " --" + o.long
		}
		if o.metavar != "" {
			syn += "=" + o.metavar
		}
		opts = append(opts, [2]string{syn, o.help})
	}
	width := 0
	for _, r := range append(cmds, opts...) {
		width = max(width, len(r[0]))
	}
	cols := columns()

	var b strings.Builder
	fmt.Fprintf(&b, "> %s [OPTION…] COMMAND …\n\n", prog)
	for _, l := range wrapWords(abstract(c.cfg.class), min(cols, 80)) {
		b.WriteString(l + "\n")
	}
	for _, section := range []struct {
		title string
		rows  [][2]string
	}{{"Commands", cmds}, {"Options", opts}} {
		fmt.Fprintf(&b, "\n%s:\n", section.title)
		for _, r := range section.rows {
			for i, l := range wrapWords(r[1], max(cols-width-1, 20)) {
				syn := ""
				if i == 0 {
					syn = r[0]
				}
				fmt.Fprintf(&b, "%-*s %s\n", width, syn, l)
			}
		}
	}
	fmt.Fprintf(&b, "\nSee the systemd-%s(8) man page for details.\n", classIdentifier(c.cfg.class))
	fmt.Fprint(c.stdout, b.String())
}

// introspection is the CLI description --introspect-cli prints, in the
// format of the UAPI CLI introspection specification systemd implements.
func introspection() jsonObject {
	opts := make([]any, 0, len(options))
	for _, o := range options {
		names := []string{"--" + o.long}
		if o.short != 0 {
			names = []string{"-" + string(o.short), "--" + o.long}
		}
		obj := jsonObject{{"names", names}}
		if o.metavar == "" {
			obj = append(obj, jsonField{"argument", "no_argument"})
		} else {
			obj = append(obj, jsonField{"argument", "required_argument"}, jsonField{"metavar", o.metavar})
		}
		if o.help != "" {
			obj = append(obj, jsonField{"help", o.help})
		}
		opts = append(opts, obj)
	}
	vs := make([]any, 0, len(verbs))
	for i, v := range verbs {
		obj := jsonObject{{"names", []string{v.name}}}
		if v.help != "" {
			obj = append(obj, jsonField{"abstract", []string{v.help}}, jsonField{"maxArguments", int64(1)})
		}
		if i == 0 {
			obj = append(obj, jsonField{"isDefault", true})
		}
		vs = append(vs, obj)
	}
	var cmds []any
	for _, class := range []release.Class{release.Sysext, release.Confext} {
		cmds = append(cmds, jsonObject{
			{"names", []string{classIdentifier(class)}},
			{"project", "sysext-alpine"},
			{"version", version},
			{"abstract", []string{abstract(class)}},
			{"options", opts},
			{"verbs", vs},
		})
	}
	return jsonObject{
		{"mediaType", "application/vnd.io.systemd.cli-introspection-0"},
		{"commands", cmds},
	}
}
