// Package dockerfile turns a Dockerfile into hull build stages.
//
// It supports the instructions most application images use: FROM (with AS),
// ARG, ENV, RUN, COPY and ADD (local files, with --from), WORKDIR, EXPOSE,
// CMD, ENTRYPOINT, USER and LABEL. Other instructions are reported as warnings
// and skipped.
package dockerfile

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/shubhamstrength/hull/internal/config"
)

// Result is a parsed Dockerfile.
type Result struct {
	Stages   []config.Stage
	Warnings []string
}

// ParseFile parses the Dockerfile at path.
func ParseFile(path string, args map[string]string) (*Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f, args)
}

type line struct {
	no   int
	text string
}

// Parse parses Dockerfile text. args override ARG defaults.
func Parse(r io.Reader, args map[string]string) (*Result, error) {
	lines, err := logicalLines(r)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	vars := map[string]string{} // ARG and ENV values visible for substitution
	stageArgs := map[string]string{}
	globalArgs := map[string]string{}
	var cur *config.Stage

	for _, l := range lines {
		instr, rest, _ := strings.Cut(l.text, " ")
		instr = strings.ToUpper(instr)
		rest = strings.TrimSpace(rest)
		errf := func(format string, a ...any) error {
			return fmt.Errorf("Dockerfile line %d: %s: %s", l.no, instr, fmt.Sprintf(format, a...))
		}

		if instr != "FROM" && instr != "ARG" && cur == nil {
			return nil, errf("must come after FROM")
		}
		switch instr {
		case "FROM":
			rest = expand(rest, globalArgs)
			fields := strings.Fields(rest)
			fields = dropFlags(fields, res, l.no)
			if len(fields) == 0 {
				return nil, errf("missing image")
			}
			st := config.Stage{From: fields[0]}
			if len(fields) == 3 && strings.EqualFold(fields[1], "AS") {
				st.Name = fields[2]
			} else if len(fields) != 1 {
				return nil, errf("expected FROM <image> [AS <name>]")
			}
			res.Stages = append(res.Stages, st)
			cur = &res.Stages[len(res.Stages)-1]
			vars = map[string]string{}
			stageArgs = map[string]string{}
		case "ARG":
			name, def, hasDef := strings.Cut(rest, "=")
			name = strings.TrimSpace(name)
			val := def
			if v, ok := args[name]; ok {
				val = v
			} else if !hasDef {
				val = globalArgs[name]
			}
			if cur == nil {
				globalArgs[name] = val
			} else {
				vars[name] = val
				stageArgs[name] = val
			}
		case "ENV":
			kv, err := parseKV(expand(rest, vars), true)
			if err != nil {
				return nil, errf("%v", err)
			}
			for k, v := range kv {
				vars[k] = v
			}
			cur.Steps = append(cur.Steps, config.Step{Env: kv})
		case "LABEL":
			kv, err := parseKV(expand(rest, vars), false)
			if err != nil {
				return nil, errf("%v", err)
			}
			cur.Steps = append(cur.Steps, config.Step{Label: kv})
		case "RUN":
			if strings.HasPrefix(rest, "--") {
				res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: RUN flags are ignored", l.no))
				for strings.HasPrefix(rest, "--") {
					_, rest, _ = strings.Cut(rest, " ")
					rest = strings.TrimSpace(rest)
				}
			}
			cmd := rest
			if arr, ok := jsonArray(rest); ok {
				cmd = shellJoin(arr)
			}
			// As with Docker, ARG values are visible to RUN as environment
			// variables but are not saved in the image.
			if len(stageArgs) > 0 {
				var exports []string
				for _, k := range sortedKeys(stageArgs) {
					exports = append(exports, k+"="+shellQuote(stageArgs[k]))
				}
				cmd = "export " + strings.Join(exports, " ") + "; " + cmd
			}
			cur.Steps = append(cur.Steps, config.Step{Run: cmd})
		case "COPY", "ADD":
			fields, err := splitArgs(expand(rest, vars))
			if err != nil {
				return nil, errf("%v", err)
			}
			cp := &config.Copy{}
			var paths []string
			for _, f := range fields {
				switch {
				case strings.HasPrefix(f, "--from="):
					cp.From = strings.TrimPrefix(f, "--from=")
				case strings.HasPrefix(f, "--"):
					res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: %s flag %s is ignored", l.no, instr, f))
				default:
					paths = append(paths, f)
				}
			}
			if len(paths) < 2 {
				return nil, errf("needs at least one source and a destination")
			}
			for _, p := range paths[:len(paths)-1] {
				if strings.Contains(p, "://") {
					return nil, errf("remote URLs are not supported; download the file first")
				}
			}
			cp.Src = paths[:len(paths)-1]
			cp.Dest = paths[len(paths)-1]
			if cp.From != "" && !stageExists(res.Stages[:len(res.Stages)-1], cp.From) {
				return nil, errf("--from=%s is not an earlier stage (copying from images is not supported)", cp.From)
			}
			cur.Steps = append(cur.Steps, config.Step{Copy: cp})
		case "WORKDIR":
			cur.Steps = append(cur.Steps, config.Step{Workdir: expand(rest, vars)})
		case "USER":
			cur.Steps = append(cur.Steps, config.Step{User: expand(rest, vars)})
		case "EXPOSE":
			cur.Steps = append(cur.Steps, config.Step{Expose: strings.Fields(expand(rest, vars))})
		case "CMD", "ENTRYPOINT":
			cmd := config.Command{"/bin/sh", "-c", rest}
			if arr, ok := jsonArray(rest); ok {
				cmd = arr
			}
			if instr == "CMD" {
				cur.Steps = append(cur.Steps, config.Step{Cmd: cmd})
			} else {
				cur.Steps = append(cur.Steps, config.Step{Entrypoint: cmd})
			}
		default:
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: %s is not supported and was skipped", l.no, instr))
		}
	}
	if len(res.Stages) == 0 {
		return nil, fmt.Errorf("Dockerfile has no FROM")
	}
	return res, nil
}

func stageExists(stages []config.Stage, name string) bool {
	for _, s := range stages {
		if s.Name == name {
			return true
		}
	}
	return false
}

func dropFlags(fields []string, res *Result, no int) []string {
	var out []string
	for _, f := range fields {
		if strings.HasPrefix(f, "--") {
			res.Warnings = append(res.Warnings, fmt.Sprintf("line %d: FROM flag %s is ignored (set platform in hull.yaml)", no, f))
			continue
		}
		out = append(out, f)
	}
	return out
}

// logicalLines joins backslash continuations and drops comments and blanks.
func logicalLines(r io.Reader) ([]line, error) {
	var out []line
	var buf strings.Builder
	start := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	no := 0
	for sc.Scan() {
		no++
		t := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(t, "#") {
			continue
		}
		if t == "" && buf.Len() == 0 {
			continue
		}
		if buf.Len() == 0 {
			start = no
		}
		if strings.HasSuffix(t, "\\") {
			buf.WriteString(strings.TrimSuffix(t, "\\"))
			buf.WriteString(" ")
			continue
		}
		buf.WriteString(t)
		out = append(out, line{no: start, text: strings.TrimSpace(buf.String())})
		buf.Reset()
	}
	if buf.Len() > 0 {
		out = append(out, line{no: start, text: strings.TrimSpace(buf.String())})
	}
	return out, sc.Err()
}

func jsonArray(s string) ([]string, bool) {
	if !strings.HasPrefix(s, "[") {
		return nil, false
	}
	var arr []string
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return nil, false
	}
	return arr, true
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// expand substitutes $VAR and ${VAR} (and ${VAR:-default}) from vars.
func expand(s string, vars map[string]string) string {
	return os.Expand(s, func(key string) string {
		if name, def, ok := strings.Cut(key, ":-"); ok {
			if v := vars[name]; v != "" {
				return v
			}
			return def
		}
		if v, ok := vars[key]; ok {
			return v
		}
		return "${" + key + "}"
	})
}

// parseKV parses "k=v k2=v2" (quotes allowed) or, if legacy is true, "k v".
func parseKV(s string, legacy bool) (map[string]string, error) {
	out := map[string]string{}
	fields, err := splitArgs(s)
	if err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return nil, fmt.Errorf("expected key=value")
	}
	if legacy && !strings.Contains(fields[0], "=") {
		k, v, _ := strings.Cut(s, " ")
		out[k] = strings.TrimSpace(v)
		return out, nil
	}
	for _, f := range fields {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return nil, fmt.Errorf("expected key=value, got %q", f)
		}
		out[k] = v
	}
	return out, nil
}

// splitArgs splits on whitespace, honouring single and double quotes and
// backslash escapes. JSON array form is also accepted.
func splitArgs(s string) ([]string, error) {
	if arr, ok := jsonArray(s); ok {
		return arr, nil
	}
	var out []string
	var cur strings.Builder
	inWord := false
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && quote != '\'':
			esc = true
			inWord = true
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, nil
}
