// Package ignore implements .hullignore / .dockerignore matching.
//
// Patterns are matched against slash-separated paths relative to the build
// context. "*" and "?" match within one path segment, "**" matches any number
// of segments, a leading "!" re-includes a path, and the last matching pattern
// wins. A pattern that matches a directory excludes everything under it.
package ignore

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type rule struct {
	re     *regexp.Regexp
	negate bool
}

// Matcher decides whether a context path is excluded.
type Matcher struct{ rules []rule }

// Load reads .hullignore from dir, or .dockerignore if there is no .hullignore.
// A missing file yields a matcher that excludes nothing.
func Load(dir string) (*Matcher, error) {
	for _, name := range []string{".hullignore", ".dockerignore"} {
		f, err := os.Open(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		var lines []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			lines = append(lines, sc.Text())
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
		return New(lines), nil
	}
	return New(nil), nil
}

// New builds a matcher from pattern lines. Blank lines and # comments are skipped.
func New(lines []string) *Matcher {
	m := &Matcher{}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		neg := strings.HasPrefix(l, "!")
		if neg {
			l = strings.TrimSpace(l[1:])
		}
		l = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(l)), "/")
		if l == "." || l == "" {
			continue
		}
		m.rules = append(m.rules, rule{re: compile(l), negate: neg})
	}
	return m
}

func compile(p string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '*' && i+1 < len(p) && p[i+1] == '*':
			i++
			if i+1 < len(p) && p[i+1] == '/' {
				i++
				b.WriteString("(.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	// Matching a directory also matches everything inside it.
	b.WriteString("(/.*)?$")
	return regexp.MustCompile(b.String())
}

// Excluded reports whether rel (relative to the context) should be left out.
func (m *Matcher) Excluded(rel string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	out := false
	for _, r := range m.rules {
		if r.re.MatchString(rel) {
			out = !r.negate
		}
	}
	return out
}
