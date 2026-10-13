package route

import (
	"reflect"
	"testing"
)

// Linux nodes: folders are POSIX paths, case-sensitive, separated by
// "/"; Windows folders keep their rules. The browser's folderKey mirrors
// normFolder (web/tests/unit/folderkey.test.ts).
func TestPosixFolders(t *testing.T) {
	for in, want := range map[string]string{
		"/home/Alice/claudeproject/":  "/home/Alice/claudeproject",
		"/home//Alice/./work/../work": "/home/Alice/work",
		"/":                           "/",
		`C:\Users\Me\Proj\`:           `c:\users\me\proj`,
		"//server/share/x":            `server\share\x`, // a UNC path keeps its Windows key, as before
	} {
		if got := normFolder(in); got != want {
			t.Errorf("normFolder(%q) = %q, want %q", in, got, want)
		}
	}

	cases := []struct {
		cwd     string
		folders []string
		in      bool
	}{
		{"/home/Alice/claudeproject/sub", []string{"/home/Alice/claudeproject"}, true},
		{"/home/Alice/claudeproject", []string{"/home/Alice/claudeproject/"}, true},
		{"/home/Alice/claudeprojectX", []string{"/home/Alice/claudeproject"}, false},
		{"/home/alice/claudeproject", []string{"/home/Alice/claudeproject"}, false}, // case matters on Linux
		{"/home/Alice/work/../claudeproject", []string{"/home/Alice/work"}, false},
		{"/anything", []string{"/"}, true},
		{`C:\Users\Alice\work`, []string{"/home/Alice/work"}, false},
		{"/home/Alice/work", []string{`C:\home\Alice\work`}, false},
		{`C:\Work\Sub`, []string{`c:\work`}, true}, // Windows unchanged
	}
	for _, c := range cases {
		if got := inFolders(c.cwd, c.folders); got != c.in {
			t.Errorf("inFolders(%q, %q) = %v, want %v", c.cwd, c.folders, got, c.in)
		}
	}

	got, err := cleanFolders([]string{" /home/Alice/work/ ", "/home/Alice/work", "/home/Alice/WORK", `C:\work\`, "/home/a/../b"})
	if want := []string{"/home/Alice/work", "/home/Alice/WORK", `C:\work`, "/home/b"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanFolders = %q, %v; want %q", got, err, want)
	}
	for _, bad := range []string{"home/x", "/a\nb", "relative"} {
		if _, err := cleanFolders([]string{bad}); err == nil {
			t.Errorf("cleanFolders(%q) accepted", bad)
		}
	}
}

func TestPosixPasteAndJoin(t *testing.T) {
	for in, want := range map[string]string{
		"/home/u/.local/share/termhub/agent/paste/x/1.png": "/home/u/.local/share/termhub/agent/paste/x/1.png ",
		"/home/u/a b.png":    "'/home/u/a b.png' ",
		"/home/u/it's$x.png": `'/home/u/it'\''s$x.png' `,
		`C:\a b\c.png`:       `"C:\a b\c.png" `,
		`C:\ab\c.png`:        `C:\ab\c.png `,
	} {
		if got := pasteText(in, "auto"); got != want {
			t.Errorf("pasteText(%q) = %q, want %q", in, got, want)
		}
	}
	if got := joinNodePath("/home/u/", "f"); got != "/home/u/f" {
		t.Errorf("join posix: %q", got)
	}
	if got := joinNodePath(`C:\u`, "f"); got != `C:\u\f` {
		t.Errorf("join windows: %q", got)
	}
}
