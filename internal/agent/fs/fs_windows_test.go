//go:build windows

package fs

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "other:" + err.Error()
}

func TestCleanPath(t *testing.T) {
	good := map[string]string{
		`C:\Users\me`:      `C:\Users\me`,
		`c:/Users/me/`:     `C:\Users\me`,
		`C:\a\.\b\..\c`:    `C:\a\c`,
		`C:\..\..\Windows`: `C:\Windows`, // cannot climb above the drive root
		`D:\`:              `D:\`,
		`C:\项目\新建 文件夹`:     `C:\项目\新建 文件夹`,
	}
	for in, want := range good {
		got, err := CleanPath(in, Options{})
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := map[string]string{
		``:                                 CodePathInvalid,
		`relative\path`:                    CodePathInvalid,
		`\rooted`:                          CodePathInvalid,
		`C:`:                               CodePathInvalid,
		`C:drive-relative`:                 CodePathInvalid,
		`\\?\C:\Windows`:                   CodePathInvalid,
		`\\.\PhysicalDrive0`:               CodePathInvalid,
		`\??\C:\x`:                         CodePathInvalid,
		`C:\file.txt:stream`:               CodePathInvalid,
		"C:\\a\x00b":                       CodePathInvalid,
		"C:\\a\nb":                         CodePathInvalid,
		`\\server\share\x`:                 CodeUNCDisabled,
		`1:\x`:                             CodePathInvalid,
		`C:\` + strings.Repeat("a", 33000): CodePathInvalid,
	}
	for in, want := range bad {
		if _, err := CleanPath(in, Options{}); code(err) != want {
			t.Errorf("CleanPath(%.40q): got %q, want %q", in, code(err), want)
		}
	}
	if got, err := CleanPath(`\\server\share\dir\..\x`, Options{AllowUNC: true}); err != nil || got != `\\server\share\x` {
		t.Errorf("UNC when allowed: %q %v", got, err)
	}
	if _, err := CleanPath(`\\server`, Options{AllowUNC: true}); code(err) != CodePathInvalid {
		t.Errorf("UNC without a share: %v", err)
	}
}

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"project", "我的项目", "a.b.c", "con-fig", "COM10", ".git", "name with space"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("CheckName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a:b", "a*b", "a?b", `a"b`, "a<b", "a>b", "a|b", "tab\tname",
		"trailing ", "trailing.", "CON", "con", "NUL.txt", "com1", "LPT9.log", "aux ", strings.Repeat("x", 256)} {
		if err := CheckName(bad); code(err) != CodeNameInvalid {
			t.Errorf("CheckName(%q) accepted: %v", bad, err)
		}
	}
}

func TestListStatMkdir(t *testing.T) {
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "zeta"), 0o755)
	os.Mkdir(filepath.Join(dir, "Alpha"), 0o755)
	os.WriteFile(filepath.Join(dir, "beta.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(dir, "hidden.txt"), nil, 0o644)
	exec.Command("attrib", "+h", filepath.Join(dir, "hidden.txt")).Run()

	l, err := List(dir, 0, false, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if strings.Join(names, ",") != "Alpha,zeta,beta.txt" {
		t.Fatalf("folders first, case-insensitive order, hidden left out: %v", names)
	}
	if l.Entries[2].Size != 5 || l.Entries[2].Dir || l.Parent != filepath.Dir(l.Path) {
		t.Fatalf("entry details: %+v parent %q", l.Entries[2], l.Parent)
	}
	if l, _ := List(dir, 0, true, Options{}); l.Total != 4 {
		t.Fatalf("with hidden: %d", l.Total)
	}

	full, err := Mkdir(dir, "新项目", Options{})
	if err != nil || full != filepath.Join(l.Path, "新项目") {
		t.Fatalf("mkdir: %q %v", full, err)
	}
	if _, err := Mkdir(dir, "新项目", Options{}); code(err) != CodeExists {
		t.Fatalf("mkdir twice: %v", err)
	}
	if _, err := Mkdir(dir, `..\escape`, Options{}); code(err) != CodeNameInvalid {
		t.Fatalf("a name must not be a path: %v", err)
	}
	if _, err := Mkdir(filepath.Join(dir, "beta.txt"), "x", Options{}); code(err) != CodeNotDirectory {
		t.Fatalf("mkdir inside a file: %v", err)
	}
	if _, err := Mkdir(filepath.Join(dir, "missing"), "x", Options{}); code(err) != CodeNotFound {
		t.Fatalf("mkdir in a missing parent: %v", err)
	}
	if _, err := List(filepath.Join(dir, "beta.txt"), 0, false, Options{}); code(err) != CodeNotDirectory {
		t.Fatalf("list a file: %v", err)
	}
	if e, err := Stat(filepath.Join(dir, "beta.txt"), Options{}); err != nil || e.Size != 5 || e.Dir {
		t.Fatalf("stat: %+v %v", e, err)
	}
	if _, err := Stat(filepath.Join(dir, "nope"), Options{}); code(err) != CodeNotFound {
		t.Fatalf("stat missing: %v", err)
	}
}

func TestListPagingAndLongPaths(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < PageSize+250; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", i)), nil, 0o644)
	}
	p0, err := List(dir, 0, false, Options{})
	if err != nil || len(p0.Entries) != PageSize || p0.Pages != 2 || p0.Total != PageSize+250 {
		t.Fatalf("page 0: %d entries, %d pages, %v", len(p0.Entries), p0.Pages, err)
	}
	p1, _ := List(dir, 1, false, Options{})
	if len(p1.Entries) != 250 || p1.Entries[0].Name != fmt.Sprintf("f%05d", PageSize) {
		t.Fatalf("page 1: %d entries, first %q", len(p1.Entries), p1.Entries[0].Name)
	}
	if p9, _ := List(dir, 9, false, Options{}); len(p9.Entries) != 0 {
		t.Fatal("a page past the end must be empty, not an error")
	}
	// Beyond MAX_PATH.
	deep := dir
	for len(deep) < 320 {
		var err error
		if deep, err = Mkdir(deep, strings.Repeat("d", 60), Options{}); err != nil {
			t.Fatalf("deep mkdir at %d chars: %v", len(deep), err)
		}
	}
	if l, err := List(deep, 0, false, Options{}); err != nil || l.Total != 0 {
		t.Fatalf("list beyond 260 characters: %v", err)
	}
}

func TestDrives(t *testing.T) {
	ds := Drives()
	found := false
	for _, d := range ds {
		if d.Letter == "C:" && d.Kind == "fixed" && d.Total > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("C: not reported as a fixed drive with a size: %+v", ds)
	}
}

// The picker's 桌面 button: the desktop is a folder that exists and that List
// can open, in the same form the drive buttons lead to.
func TestDesktop(t *testing.T) {
	d := Desktop()
	if d == "" || !filepath.IsAbs(d) {
		t.Fatalf("desktop not found: %q", d)
	}
	if _, err := List(d, 0, false, Options{}); err != nil {
		t.Fatalf("list desktop %s: %v", d, err)
	}
}

// A desktop registered inside SYSTEM's profile falls back to the one in the
// user's own profile; a user whose profile is itself there gets none.
func TestPickDesktop(t *testing.T) {
	win := `C:\WINDOWS`
	for _, c := range []struct{ known, home, want string }{
		{`D:\OneDrive\桌面`, `C:\Users\me`, `D:\OneDrive\桌面`},
		{`C:\Windows\system32\config\systemprofile\Desktop`, `C:\Users\me`, `C:\Users\me\Desktop`},
		{``, `C:\Users\me`, `C:\Users\me\Desktop`},
		{`C:\WINDOWS\system32\config\systemprofile\Desktop`, `C:\WINDOWS\system32\config\systemprofile`, ``},
		{`C:\WindowsApps\Desktop`, ``, `C:\WindowsApps\Desktop`},
	} {
		if got := pickDesktop(c.known, c.home, win); got != c.want {
			t.Errorf("pickDesktop(%q, %q) = %q, want %q", c.known, c.home, got, c.want)
		}
	}
}
