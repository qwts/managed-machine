package statedir

import (
	"path/filepath"
	"testing"
)

func TestPath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		goos string
		vars map[string]string
		want string
	}{
		{"darwin", map[string]string{"HOME": "/h"}, filepath.Join("/h", "Library", "Application Support", Name)},
		{"linux", map[string]string{"HOME": "/h"}, filepath.Join("/h", ".local", "share", Name)},
		{"linux", map[string]string{"HOME": "/h", "XDG_DATA_HOME": "/x"}, filepath.Join("/x", Name)},
		{"linux", map[string]string{"HOME": "/h", "XDG_DATA_HOME": "rel"}, filepath.Join("/h", ".local", "share", Name)},
		{"windows", map[string]string{"LOCALAPPDATA": `C:\L`}, filepath.Join(`C:\L`, Name)},
		{"darwin", map[string]string{"HOME": "/h", Override: "/o"}, filepath.Clean("/o")},
	}
	for _, c := range cases {
		got, err := Path(c.goos, env(c.vars))
		if err != nil || got != c.want {
			t.Errorf("%s %v: %q %v, want %q", c.goos, c.vars, got, err, c.want)
		}
	}
	if _, err := Path("windows", env(map[string]string{"HOME": "/h"})); err != ErrNoHome {
		t.Errorf("windows without LOCALAPPDATA: %v", err)
	}
}
