package ignore

import "testing"

func TestExcluded(t *testing.T) {
	m := New([]string{
		"# comment",
		"node_modules",
		"*.log",
		"**/.DS_Store",
		"dist/",
		"secrets/*",
		"!secrets/keep.txt",
	})
	cases := map[string]bool{
		"node_modules":                true,
		"node_modules/react/index.js": true,
		"src/node_modules":            false,
		"npm-debug.log":               true,
		"logs/app.log":                false,
		"a/b/.DS_Store":               true,
		".DS_Store":                   true,
		"dist/index.html":             true,
		"secrets/key.pem":             true,
		"secrets/keep.txt":            false,
		"src/App.jsx":                 false,
		"package.json":                false,
	}
	for path, want := range cases {
		if got := m.Excluded(path); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", path, got, want)
		}
	}
}
