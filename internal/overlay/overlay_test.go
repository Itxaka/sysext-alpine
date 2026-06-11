package overlay

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/itxaka/sysext-alpine/internal/release"
)

func TestErrAlreadyMergedSentinel(t *testing.T) {
	if ErrAlreadyMerged == nil {
		t.Fatal("ErrAlreadyMerged must be a non-nil sentinel")
	}
	wrapped := errors.Join(ErrAlreadyMerged)
	if !errors.Is(wrapped, ErrAlreadyMerged) {
		t.Fatal("ErrAlreadyMerged must survive errors.Is through wrapping")
	}
}

func TestHierarchies(t *testing.T) {
	if got := Hierarchies(release.Sysext); !reflect.DeepEqual(got, []string{"/usr", "/opt"}) {
		t.Errorf("Sysext hierarchies = %v", got)
	}
	if got := Hierarchies(release.Confext); !reflect.DeepEqual(got, []string{"/etc"}) {
		t.Errorf("Confext hierarchies = %v", got)
	}
}

func TestParseHierarchiesEnv(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		// valid lists
		{"/usr", []string{"/usr"}, false},
		{"/usr:/opt", []string{"/usr", "/opt"}, false},
		{"/usr:/opt:/srv", []string{"/usr", "/opt", "/srv"}, false},
		{"/etc:/srv/conf.d", []string{"/etc", "/srv/conf.d"}, false},
		// empty string → nil, nil (caller keeps defaults)
		{"", nil, false},
		// relative entry
		{"usr", nil, true},
		{"/usr:opt", nil, true},
		{"./usr", nil, true},
		// root is not allowed
		{"/", nil, true},
		{"/usr:/", nil, true},
		// not a cleaned path
		{"/usr/", nil, true},
		{"/usr//local", nil, true},
		{"/usr/../etc", nil, true},
		// duplicates
		{"/usr:/usr", nil, true},
		{"/usr:/opt:/usr", nil, true},
		// empty entries
		{":", nil, true},
		{"/usr:", nil, true},
		{":/usr", nil, true},
	}
	for _, c := range cases {
		got, err := parseHierarchiesEnv(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseHierarchiesEnv(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseHierarchiesEnv(%q): unexpected error %v", c.in, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseHierarchiesEnv(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestHierarchiesEnvOverride(t *testing.T) {
	t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", "/usr:/opt:/srv")
	if got := Hierarchies(release.Sysext); !reflect.DeepEqual(got, []string{"/usr", "/opt", "/srv"}) {
		t.Errorf("overridden sysext hierarchies = %v", got)
	}
	// the sysext variable must not leak into confext
	if got := Hierarchies(release.Confext); !reflect.DeepEqual(got, []string{"/etc"}) {
		t.Errorf("confext hierarchies with sysext override = %v", got)
	}

	t.Setenv("SYSTEMD_CONFEXT_HIERARCHIES", "/etc:/srv/conf")
	if got := Hierarchies(release.Confext); !reflect.DeepEqual(got, []string{"/etc", "/srv/conf"}) {
		t.Errorf("overridden confext hierarchies = %v", got)
	}

	// empty value → defaults
	t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", "")
	if got := Hierarchies(release.Sysext); !reflect.DeepEqual(got, []string{"/usr", "/opt"}) {
		t.Errorf("empty env sysext hierarchies = %v", got)
	}
}

func TestHierarchiesEnvInvalidFallsBack(t *testing.T) {
	for _, bad := range []string{"relative/path", "/", "/usr:/usr", "/usr/", "/usr:"} {
		t.Setenv("SYSTEMD_SYSEXT_HIERARCHIES", bad)
		if got := Hierarchies(release.Sysext); !reflect.DeepEqual(got, []string{"/usr", "/opt"}) {
			t.Errorf("env %q: sysext hierarchies = %v, want defaults", bad, got)
		}
		t.Setenv("SYSTEMD_CONFEXT_HIERARCHIES", bad)
		if got := Hierarchies(release.Confext); !reflect.DeepEqual(got, []string{"/etc"}) {
			t.Errorf("env %q: confext hierarchies = %v, want defaults", bad, got)
		}
	}
}

func TestWorkspace(t *testing.T) {
	cases := []struct {
		class release.Class
		root  string
		want  string
	}{
		{release.Sysext, "", "/run/systemd/sysext"},
		{release.Sysext, "/", "/run/systemd/sysext"},
		{release.Confext, "", "/run/systemd/confext"},
		{release.Sysext, "/alt/root", "/alt/root/run/systemd/sysext"},
		{release.Confext, "/alt/root/", "/alt/root/run/systemd/confext"},
	}
	for _, c := range cases {
		if got := Workspace(c.class, c.root); got != c.want {
			t.Errorf("Workspace(%v, %q) = %q, want %q", c.class, c.root, got, c.want)
		}
	}
}

func TestMarkerDirName(t *testing.T) {
	if got := MarkerDirName(release.Sysext); got != ".systemd-sysext" {
		t.Errorf("sysext marker = %q", got)
	}
	if got := MarkerDirName(release.Confext); got != ".systemd-confext" {
		t.Errorf("confext marker = %q", got)
	}
}

func TestEscapeHierarchy(t *testing.T) {
	cases := map[string]string{
		"/usr":            "usr",
		"/opt":            "opt",
		"/etc":            "etc",
		"/some/deep/path": "some-deep-path",
		"/usr/":           "usr",
	}
	for in, want := range cases {
		if got := escapeHierarchy(in); got != want {
			t.Errorf("escapeHierarchy(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildLowerdir(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"/a", "/b", "/c"}, "/a:/b:/c"},
		{[]string{"/run/systemd/sysext/meta/usr", "/usr"}, "/run/systemd/sysext/meta/usr:/usr"},
		// overlayfs option escaping: ':' ',' '\' get backslash-escaped
		{[]string{"/with:colon", "/plain"}, `/with\:colon:/plain`},
		{[]string{"/with,comma"}, `/with\,comma`},
		{[]string{`/with\backslash`}, `/with\\backslash`},
		{[]string{`/a:b,c\d`, "/e"}, `/a\:b\,c\\d:/e`},
		{nil, ""},
	}
	for _, c := range cases {
		if got := buildLowerdir(c.in); got != c.want {
			t.Errorf("buildLowerdir(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestOverlayMountFlags(t *testing.T) {
	const (
		msRdonly = 0x1
		msNosuid = 0x2
		msNodev  = 0x4
		msNoexec = 0x8
	)
	if got := overlayMountFlags(release.Sysext, false); got != msRdonly|msNodev {
		t.Errorf("sysext flags = %#x", got)
	}
	// sysext never gets noexec, regardless of the option
	if got := overlayMountFlags(release.Sysext, true); got != msRdonly|msNodev {
		t.Errorf("sysext flags (noexec opt) = %#x", got)
	}
	if got := overlayMountFlags(release.Confext, true); got != msRdonly|msNodev|msNosuid|msNoexec {
		t.Errorf("confext flags (noexec) = %#x", got)
	}
	if got := overlayMountFlags(release.Confext, false); got != msRdonly|msNodev|msNosuid {
		t.Errorf("confext flags (no noexec) = %#x", got)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	markerDir := filepath.Join(dir, "meta", "usr", ".systemd-sysext")

	names := []string{"foo", "bar-1.2", "baz"}
	origins := []originEntry{
		{Name: "foo", Path: "/var/lib/extensions/foo.raw", Type: "raw"},
		{Name: "bar-1.2", Path: "/var/lib/extensions/bar-1.2", Type: "directory"},
		{Name: "baz", Path: "/run/extensions/baz.raw", Type: "raw"},
	}
	if err := writeMarker(markerDir, names, origins); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}

	gotNames, err := readExtensionsFile(filepath.Join(markerDir, "extensions"))
	if err != nil {
		t.Fatalf("readExtensionsFile: %v", err)
	}
	if !reflect.DeepEqual(gotNames, names) {
		t.Errorf("extensions round-trip = %v, want %v", gotNames, names)
	}

	gotOrigins, err := readOriginFile(filepath.Join(markerDir, "origin"))
	if err != nil {
		t.Fatalf("readOriginFile: %v", err)
	}
	if !reflect.DeepEqual(gotOrigins, origins) {
		t.Errorf("origin round-trip = %+v, want %+v", gotOrigins, origins)
	}

	// raw content sanity: newline-delimited list ending in newline
	raw, err := os.ReadFile(filepath.Join(markerDir, "extensions"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "foo\nbar-1.2\nbaz\n" {
		t.Errorf("extensions file content = %q", raw)
	}
}

func TestCollectRawOriginPaths(t *testing.T) {
	ws := t.TempDir()
	marker := MarkerDirName(release.Sysext)
	if err := writeMarker(filepath.Join(ws, "meta", "usr", marker),
		[]string{"a", "b"},
		[]originEntry{
			{Name: "a", Path: "/x/a.raw", Type: "raw"},
			{Name: "b", Path: "/x/b", Type: "directory"},
		}); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(filepath.Join(ws, "meta", "opt", marker),
		[]string{"a"},
		[]originEntry{{Name: "a", Path: "/x/a.raw", Type: "raw"}}); err != nil {
		t.Fatal(err)
	}
	got := collectRawOriginPaths(release.Sysext, ws)
	if !reflect.DeepEqual(got, []string{"/x/a.raw"}) {
		t.Errorf("collectRawOriginPaths = %v, want [/x/a.raw] (raw only, deduped)", got)
	}
}

func TestIsMountPointNegative(t *testing.T) {
	dir := t.TempDir()

	// plain subdirectory of the same filesystem → not a mount point
	sub := filepath.Join(dir, "usr")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	mp, err := isMountPoint(sub)
	if err != nil {
		t.Fatalf("isMountPoint(%s): %v", sub, err)
	}
	if mp {
		t.Errorf("plain dir %s reported as mount point", sub)
	}

	// missing path → false, no error
	mp, err = isMountPoint(filepath.Join(dir, "does-not-exist"))
	if err != nil || mp {
		t.Errorf("missing path: mp=%v err=%v, want false,nil", mp, err)
	}

	// root is always a mount point
	mp, err = isMountPoint("/")
	if err != nil || !mp {
		t.Errorf("/: mp=%v err=%v, want true,nil", mp, err)
	}
}

func TestIsMergedByUsNegative(t *testing.T) {
	root := t.TempDir()

	// hierarchy missing entirely
	merged, err := IsMergedByUs(release.Sysext, root, "/usr")
	if err != nil || merged {
		t.Errorf("missing hierarchy: merged=%v err=%v, want false,nil", merged, err)
	}

	// hierarchy exists but is not a mount point
	if err := os.MkdirAll(filepath.Join(root, "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	merged, err = IsMergedByUs(release.Sysext, root, "/usr")
	if err != nil || merged {
		t.Errorf("plain dir: merged=%v err=%v, want false,nil", merged, err)
	}

	// even with a marker dev file present, a non-mount-point is not merged
	markerDir := filepath.Join(root, "usr", MarkerDirName(release.Sysext))
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "dev"), []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, err = IsMergedByUs(release.Sysext, root, "/usr")
	if err != nil || merged {
		t.Errorf("marker without mount: merged=%v err=%v, want false,nil", merged, err)
	}

	// confext on the same unmerged root
	merged, err = IsMergedByUs(release.Confext, root, "/etc")
	if err != nil || merged {
		t.Errorf("confext unmerged: merged=%v err=%v, want false,nil", merged, err)
	}
}

func TestCurrentStatusUnmerged(t *testing.T) {
	root := t.TempDir()
	for _, h := range []string{"usr", "opt", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, h), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	st, err := CurrentStatus(release.Sysext, root)
	if err != nil {
		t.Fatalf("CurrentStatus: %v", err)
	}
	if len(st) != 2 || st[0].Hierarchy != "/usr" || st[1].Hierarchy != "/opt" {
		t.Fatalf("unexpected statuses: %+v", st)
	}
	for _, s := range st {
		if s.Merged || s.Extensions != nil || s.Since != 0 {
			t.Errorf("unmerged %s should be empty: %+v", s.Hierarchy, s)
		}
	}

	st, err = CurrentStatus(release.Confext, root)
	if err != nil {
		t.Fatalf("CurrentStatus(confext): %v", err)
	}
	if len(st) != 1 || st[0].Hierarchy != "/etc" || st[0].Merged {
		t.Fatalf("unexpected confext statuses: %+v", st)
	}
}

func TestMergedExtensionsUnmerged(t *testing.T) {
	root := t.TempDir()
	names, err := MergedExtensions(release.Sysext, root, "/usr")
	if err != nil {
		t.Fatalf("MergedExtensions: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("unmerged hierarchy returned extensions: %v", names)
	}
}

func TestMergeNoImagesIsNoop(t *testing.T) {
	root := t.TempDir()
	if err := Merge(release.Sysext, nil, MergeOptions{Root: root}); err != nil {
		t.Fatalf("Merge with no images should be a no-op, got %v", err)
	}
	if _, err := os.Stat(Workspace(release.Sysext, root)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no-op merge must not create the workspace")
	}
}

func TestUnmergeIdempotentOnUnmergedRoot(t *testing.T) {
	root := t.TempDir()
	if err := Unmerge(release.Sysext, root); err != nil {
		t.Fatalf("Unmerge on clean root: %v", err)
	}
	// stale workspace with markers but no mounts → removed, no error
	ws := Workspace(release.Sysext, root)
	marker := MarkerDirName(release.Sysext)
	if err := writeMarker(filepath.Join(ws, "meta", "usr", marker),
		[]string{"a"},
		[]originEntry{{Name: "a", Path: "/x/a", Type: "directory"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "extensions", "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "overlay", "usr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Unmerge(release.Sysext, root); err != nil {
		t.Fatalf("Unmerge with stale workspace: %v", err)
	}
	if _, err := os.Stat(ws); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace not removed by Unmerge")
	}
	// and again, fully idempotent
	if err := Unmerge(release.Sysext, root); err != nil {
		t.Fatalf("second Unmerge: %v", err)
	}
}

func TestNormalizeMutableMode(t *testing.T) {
	valid := map[string]string{
		"":                 "no",
		"no":               "no",
		"auto":             "auto",
		"yes":              "yes",
		"import":           "import",
		"ephemeral":        "ephemeral",
		"ephemeral-import": "ephemeral-import",
	}
	for in, want := range valid {
		got, err := normalizeMutableMode(in)
		if err != nil || got != want {
			t.Errorf("normalizeMutableMode(%q) = %q, %v; want %q, nil", in, got, err, want)
		}
	}
	for _, in := range []string{"bogus", "enabled", "disabled", "NO", "Yes", "ephemeral_import", "help"} {
		if _, err := normalizeMutableMode(in); err == nil {
			t.Errorf("normalizeMutableMode(%q) should error", in)
		}
	}
}

func TestMutableModePredicates(t *testing.T) {
	cases := []struct {
		mode      string
		ephemeral bool
		imports   bool
	}{
		{"no", false, false},
		{"auto", false, false},
		{"yes", false, false},
		{"import", false, true},
		{"ephemeral", true, false},
		{"ephemeral-import", true, true},
	}
	for _, c := range cases {
		if got := modeUsesEphemeralUpper(c.mode); got != c.ephemeral {
			t.Errorf("modeUsesEphemeralUpper(%q) = %v, want %v", c.mode, got, c.ephemeral)
		}
		if got := modeImportsRouting(c.mode); got != c.imports {
			t.Errorf("modeImportsRouting(%q) = %v, want %v", c.mode, got, c.imports)
		}
	}
}

func TestRoutingDir(t *testing.T) {
	cases := []struct {
		root, hierarchy, want string
	}{
		{"", "/usr", "/var/lib/extensions.mutable/usr"},
		{"", "/opt", "/var/lib/extensions.mutable/opt"},
		{"", "/etc", "/var/lib/extensions.mutable/etc"},
		{"/alt", "/usr", "/alt/var/lib/extensions.mutable/usr"},
	}
	for _, c := range cases {
		if got := routingDir(c.root, c.hierarchy); got != c.want {
			t.Errorf("routingDir(%q, %q) = %q, want %q", c.root, c.hierarchy, got, c.want)
		}
	}
}

func TestWorkDirForUpper(t *testing.T) {
	cases := []struct {
		hierarchy, upper, want string
	}{
		// plain routing dir → hidden sibling inside the routing base
		{"/usr", "/var/lib/extensions.mutable/usr", "/var/lib/extensions.mutable/.usr-workdir"},
		{"/etc", "/var/lib/extensions.mutable/etc", "/var/lib/extensions.mutable/.etc-workdir"},
		// routing symlink resolved to the host hierarchy → sibling of target
		{"/usr", "/usr", "/.usr-workdir"},
		{"/opt", "/data/opt-writes", "/data/.opt-workdir"},
	}
	for _, c := range cases {
		if got := workDirForUpper(c.hierarchy, c.upper); got != c.want {
			t.Errorf("workDirForUpper(%q, %q) = %q, want %q", c.hierarchy, c.upper, got, c.want)
		}
	}
}

func TestBuildOverlayData(t *testing.T) {
	// read-only: lowerdir only, no mutable options
	got := buildOverlayData([]string{"/meta", "/ext", "/usr"}, "", "")
	if got != "lowerdir=/meta:/ext:/usr" {
		t.Errorf("read-only data = %q", got)
	}
	// mutable: upperdir + workdir + systemd's mutable mount options
	got = buildOverlayData([]string{"/meta", "/usr"},
		"/var/lib/extensions.mutable/usr", "/var/lib/extensions.mutable/.usr-workdir")
	want := "lowerdir=/meta:/usr" +
		",upperdir=/var/lib/extensions.mutable/usr" +
		",workdir=/var/lib/extensions.mutable/.usr-workdir" +
		",redirect_dir=on,metacopy=off,index=off"
	if got != want {
		t.Errorf("mutable data = %q, want %q", got, want)
	}
	// upper/work paths get overlayfs option escaping
	got = buildOverlayData([]string{"/meta"}, "/up,per", "/work:dir")
	if got != `lowerdir=/meta,upperdir=/up\,per,workdir=/work\:dir,redirect_dir=on,metacopy=off,index=off` {
		t.Errorf("escaped mutable data = %q", got)
	}
}

func TestHostLowerExcluded(t *testing.T) {
	cases := []struct {
		host, upper, importDir string
		want                   bool
	}{
		{"/usr", "", "", false},
		{"/usr", "/var/lib/extensions.mutable/usr", "", false},
		// upperdir IS the host hierarchy (routing symlink → /usr)
		{"/usr", "/usr", "", true},
		{"/usr", "/usr/", "", true}, // path cleaning
		// imported routing dir is the host itself
		{"/usr", "", "/usr", true},
		{"/etc", "/var/lib/extensions.mutable/etc", "/var/lib/extensions.mutable/etc", false},
	}
	for _, c := range cases {
		if got := hostLowerExcluded(c.host, c.upper, c.importDir); got != c.want {
			t.Errorf("hostLowerExcluded(%q, %q, %q) = %v, want %v", c.host, c.upper, c.importDir, got, c.want)
		}
	}
}

func TestBuildLowerPaths(t *testing.T) {
	meta := "/run/systemd/sysext/meta/usr"
	exts := []string{"/ws/ext/b/usr", "/ws/ext/a/usr"}

	// default read-only: meta : exts : host
	got := buildLowerPaths(meta, "", exts, "/usr", true, "")
	if !reflect.DeepEqual(got, []string{meta, "/ws/ext/b/usr", "/ws/ext/a/usr", "/usr"}) {
		t.Errorf("read-only lower = %v", got)
	}

	// import: routing dir directly below meta, host still at the bottom
	got = buildLowerPaths(meta, "/var/lib/extensions.mutable/usr", exts, "/usr", true, "")
	if !reflect.DeepEqual(got, []string{meta, "/var/lib/extensions.mutable/usr", "/ws/ext/b/usr", "/ws/ext/a/usr", "/usr"}) {
		t.Errorf("import lower = %v", got)
	}

	// missing/empty host omitted
	got = buildLowerPaths(meta, "", exts, "/opt", false, "")
	if !reflect.DeepEqual(got, []string{meta, "/ws/ext/b/usr", "/ws/ext/a/usr"}) {
		t.Errorf("no-host lower = %v", got)
	}

	// host serving as upperdir is excluded from lowerdir
	got = buildLowerPaths(meta, "", exts, "/usr", true, "/usr")
	if !reflect.DeepEqual(got, []string{meta, "/ws/ext/b/usr", "/ws/ext/a/usr"}) {
		t.Errorf("upper==host lower = %v", got)
	}

	// mutable upper elsewhere keeps host at the bottom
	got = buildLowerPaths(meta, "", exts, "/usr", true, "/var/lib/extensions.mutable/usr")
	if !reflect.DeepEqual(got, []string{meta, "/ws/ext/b/usr", "/ws/ext/a/usr", "/usr"}) {
		t.Errorf("mutable lower = %v", got)
	}
}

func TestResolveHierarchyMutability(t *testing.T) {
	newRoot := func(t *testing.T) (root, ws string) {
		t.Helper()
		root = t.TempDir()
		return root, Workspace(release.Sysext, root)
	}

	t.Run("invalid mode", func(t *testing.T) {
		root, ws := newRoot(t)
		if _, err := resolveHierarchyMutability("bogus", root, "/usr", ws); err == nil {
			t.Fatal("expected error for invalid mode")
		}
	})

	t.Run("no is inert even with routing dir", func(t *testing.T) {
		root, ws := newRoot(t)
		if err := os.MkdirAll(routingDir(root, "/usr"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"", "no"} {
			hm, err := resolveHierarchyMutability(mode, root, "/usr", ws)
			if err != nil {
				t.Fatalf("mode %q: %v", mode, err)
			}
			if hm != (hierMutable{}) {
				t.Errorf("mode %q: expected zero config, got %+v", mode, hm)
			}
		}
	})

	t.Run("auto without routing dir stays read-only", func(t *testing.T) {
		root, ws := newRoot(t)
		hm, err := resolveHierarchyMutability("auto", root, "/usr", ws)
		if err != nil {
			t.Fatal(err)
		}
		if hm != (hierMutable{}) {
			t.Errorf("expected zero config, got %+v", hm)
		}
		if _, err := os.Stat(routingDir(root, "/usr")); !errors.Is(err, os.ErrNotExist) {
			t.Error("auto must not create the routing dir")
		}
	})

	t.Run("auto with routing dir is mutable", func(t *testing.T) {
		root, ws := newRoot(t)
		routing := routingDir(root, "/usr")
		if err := os.MkdirAll(routing, 0o755); err != nil {
			t.Fatal(err)
		}
		hm, err := resolveHierarchyMutability("auto", root, "/usr", ws)
		if err != nil {
			t.Fatal(err)
		}
		// EvalSymlinks may canonicalize the tempdir prefix; compare via samefile.
		fiGot, err := os.Stat(hm.upperDir)
		if err != nil {
			t.Fatalf("stat upperDir %q: %v", hm.upperDir, err)
		}
		fiWant, _ := os.Stat(routing)
		if !os.SameFile(fiGot, fiWant) {
			t.Errorf("upperDir = %q, want %q", hm.upperDir, routing)
		}
		if filepath.Base(hm.workDir) != ".usr-workdir" || filepath.Dir(hm.workDir) != filepath.Dir(hm.upperDir) {
			t.Errorf("workDir = %q, want hidden sibling of %q", hm.workDir, hm.upperDir)
		}
		if fi, err := os.Stat(hm.workDir); err != nil || !fi.IsDir() {
			t.Errorf("workdir not created: %v", err)
		}
		if hm.importDir != "" || hm.tmpfs != "" {
			t.Errorf("unexpected import/tmpfs: %+v", hm)
		}
	})

	t.Run("auto with routing symlink routes to target", func(t *testing.T) {
		root, ws := newRoot(t)
		target := filepath.Join(root, "usr")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(routingDir(root, "/usr")), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, routingDir(root, "/usr")); err != nil {
			t.Fatal(err)
		}
		hm, err := resolveHierarchyMutability("auto", root, "/usr", ws)
		if err != nil {
			t.Fatal(err)
		}
		fiGot, err := os.Stat(hm.upperDir)
		if err != nil {
			t.Fatalf("stat upperDir: %v", err)
		}
		fiWant, _ := os.Stat(target)
		if !os.SameFile(fiGot, fiWant) {
			t.Errorf("upperDir = %q, want symlink target %q", hm.upperDir, target)
		}
		// workdir is a hidden sibling of the *target*, sharing its filesystem
		if filepath.Dir(hm.workDir) != filepath.Dir(hm.upperDir) {
			t.Errorf("workDir %q not a sibling of resolved upper %q", hm.workDir, hm.upperDir)
		}
	})

	t.Run("yes creates routing dir", func(t *testing.T) {
		root, ws := newRoot(t)
		hm, err := resolveHierarchyMutability("yes", root, "/opt", ws)
		if err != nil {
			t.Fatal(err)
		}
		routing := routingDir(root, "/opt")
		if fi, err := os.Stat(routing); err != nil || !fi.IsDir() {
			t.Fatalf("routing dir not created: %v", err)
		}
		if hm.upperDir == "" || hm.workDir == "" {
			t.Errorf("yes must be mutable: %+v", hm)
		}
	})

	t.Run("yes fails when routing dir cannot be created", func(t *testing.T) {
		root, ws := newRoot(t)
		// occupy the routing base path with a regular file → MkdirAll fails
		if err := os.MkdirAll(filepath.Join(root, "var/lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "var/lib/extensions.mutable"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveHierarchyMutability("yes", root, "/usr", ws); err == nil {
			t.Fatal("expected error when routing dir cannot be created")
		}
	})

	t.Run("import is read-only with routing dir as importDir", func(t *testing.T) {
		root, ws := newRoot(t)
		routing := routingDir(root, "/usr")
		if err := os.MkdirAll(routing, 0o755); err != nil {
			t.Fatal(err)
		}
		hm, err := resolveHierarchyMutability("import", root, "/usr", ws)
		if err != nil {
			t.Fatal(err)
		}
		if hm.upperDir != "" || hm.workDir != "" || hm.tmpfs != "" {
			t.Errorf("import must stay read-only: %+v", hm)
		}
		fiGot, err := os.Stat(hm.importDir)
		if err != nil {
			t.Fatalf("stat importDir: %v", err)
		}
		fiWant, _ := os.Stat(routing)
		if !os.SameFile(fiGot, fiWant) {
			t.Errorf("importDir = %q, want %q", hm.importDir, routing)
		}
	})

	t.Run("import without routing dir imports nothing", func(t *testing.T) {
		root, ws := newRoot(t)
		hm, err := resolveHierarchyMutability("import", root, "/usr", ws)
		if err != nil {
			t.Fatal(err)
		}
		if hm != (hierMutable{}) {
			t.Errorf("expected zero config, got %+v", hm)
		}
	})
}

func TestSafeWorkDirPath(t *testing.T) {
	ws := "/run/systemd/sysext"
	cases := []struct {
		p    string
		want bool
	}{
		{"/var/lib/extensions.mutable/.usr-workdir", true},
		{"/.usr-workdir", true},
		{"/run/systemd/sysext/mh_workspace/usr/work", true},
		{"", false},
		{"relative/.usr-workdir", false},
		{"/", false},
		{"/usr", false},
		{"/run/systemd/sysext/mh_workspace", false}, // the dir itself, not inside it
	}
	for _, c := range cases {
		if got := safeWorkDirPath(c.p, ws); got != c.want {
			t.Errorf("safeWorkDirPath(%q) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestMergeInvalidMutableMode(t *testing.T) {
	root := t.TempDir()
	err := Merge(release.Sysext, nil, MergeOptions{Root: root, Mutable: "bogus"})
	if err == nil {
		t.Fatal("Merge with invalid --mutable mode must fail")
	}
}

func TestDirNonEmpty(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.Mkdir(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, "full")
	if err := os.MkdirAll(filepath.Join(full, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirNonEmpty(empty) {
		t.Error("empty dir reported non-empty")
	}
	if !dirNonEmpty(full) {
		t.Error("non-empty dir reported empty")
	}
	if dirNonEmpty(file) {
		t.Error("regular file reported as non-empty dir")
	}
	if dirNonEmpty(filepath.Join(dir, "missing")) {
		t.Error("missing path reported as non-empty dir")
	}
}
