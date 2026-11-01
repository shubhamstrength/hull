// Package config loads and validates hull.yaml, the per-project build and run file.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultFile is the project file hull looks for in the current directory.
const DefaultFile = "hull.yaml"

// Project is the top level of hull.yaml.
type Project struct {
	// Name is a short name for the project, used as the local image name
	// when Image is not set.
	Name string `yaml:"name"`
	// Image is the full repository reference to push to, for example
	// ghcr.io/me/my-app or 123456789012.dkr.ecr.us-east-1.amazonaws.com/my-app.
	Image string `yaml:"image,omitempty"`
	// Tags to apply to the built image. Defaults to ["latest"].
	Tags []string `yaml:"tags,omitempty"`
	// Platform of the image, os/arch[/variant]. Defaults to linux/<host arch>.
	Platform string `yaml:"platform,omitempty"`
	// Context is the directory COPY sources are read from, relative to the
	// project file. Defaults to the project file's directory.
	Context string `yaml:"context,omitempty"`
	// Dockerfile, if set, is read instead of Stages (a common subset is supported).
	Dockerfile string `yaml:"dockerfile,omitempty"`
	// Args are build arguments substituted into a Dockerfile's ARG values.
	Args map[string]string `yaml:"args,omitempty"`
	// Stages describe the build. The last stage is the image that gets tagged.
	Stages []Stage `yaml:"stages,omitempty"`
	// Run holds the defaults used by `hull run` for this project.
	Run RunConfig `yaml:"run,omitempty"`

	// Dir is the directory the project file was loaded from (not in YAML).
	Dir string `yaml:"-"`
}

// Stage is one build stage, like a FROM block in a Dockerfile.
type Stage struct {
	Name  string `yaml:"name,omitempty"`
	From  string `yaml:"from"`
	Steps []Step `yaml:"steps,omitempty"`
}

// Step is a single build instruction. Exactly one field must be set.
type Step struct {
	Run        string            `yaml:"run,omitempty" json:"run,omitempty"`
	Copy       *Copy             `yaml:"copy,omitempty" json:"copy,omitempty"`
	Env        map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	Workdir    string            `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	Expose     StringList        `yaml:"expose,omitempty" json:"expose,omitempty"`
	Cmd        Command           `yaml:"cmd,omitempty" json:"cmd,omitempty"`
	Entrypoint Command           `yaml:"entrypoint,omitempty" json:"entrypoint,omitempty"`
	User       string            `yaml:"user,omitempty" json:"user,omitempty"`
	Label      map[string]string `yaml:"label,omitempty" json:"label,omitempty"`
}

// Copy copies files from the build context, or from an earlier stage, into the image.
type Copy struct {
	Src  StringList `yaml:"src" json:"src"`
	Dest string     `yaml:"dest" json:"dest"`
	// From names an earlier stage to copy from instead of the build context.
	From string `yaml:"from,omitempty" json:"from,omitempty"`
}

// RunConfig holds defaults for `hull run`.
type RunConfig struct {
	Ports    StringList        `yaml:"ports,omitempty"`
	Env      map[string]string `yaml:"env,omitempty"`
	Volumes  StringList        `yaml:"volumes,omitempty"`
	Cmd      Command           `yaml:"cmd,omitempty"`
	Memory   string            `yaml:"memory,omitempty"`
	CPUs     float64           `yaml:"cpus,omitempty"`
	Pids     int64             `yaml:"pids,omitempty"`
	Network  string            `yaml:"network,omitempty"`
	Hostname string            `yaml:"hostname,omitempty"`
}

// StringList accepts either a single string or a list of strings in YAML.
type StringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *StringList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*s = StringList{n.Value}
		return nil
	case yaml.SequenceNode:
		var out []string
		if err := n.Decode(&out); err != nil {
			return err
		}
		*s = out
		return nil
	}
	return fmt.Errorf("line %d: expected a string or a list of strings", n.Line)
}

// Command is a CMD or ENTRYPOINT. A plain string is "shell form" and is
// wrapped as /bin/sh -c "<string>"; a list is used as is.
type Command []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (c *Command) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*c = Command{"/bin/sh", "-c", n.Value}
		return nil
	case yaml.SequenceNode:
		var out []string
		if err := n.Decode(&out); err != nil {
			return err
		}
		*c = out
		return nil
	}
	return fmt.Errorf("line %d: expected a string or a list of strings", n.Line)
}

// Load reads and validates a project file.
func Load(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	p.Dir = filepath.Dir(abs)
	return p, nil
}

// Parse decodes and validates hull.yaml content.
func Parse(data []byte) (*Project, error) {
	var p Project
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the project for mistakes that would only show up mid-build.
func (p *Project) Validate() error {
	var errs []error
	if p.Name == "" && p.Image == "" {
		errs = append(errs, errors.New("set name or image"))
	}
	if p.Dockerfile != "" && len(p.Stages) > 0 {
		errs = append(errs, errors.New("use either dockerfile or stages, not both"))
	}
	if p.Dockerfile == "" && len(p.Stages) == 0 {
		errs = append(errs, errors.New("add at least one stage (or point dockerfile at a Dockerfile)"))
	}
	seen := map[string]bool{}
	for i, s := range p.Stages {
		where := fmt.Sprintf("stages[%d]", i)
		if s.Name != "" {
			where = fmt.Sprintf("stage %q", s.Name)
		}
		if s.From == "" {
			errs = append(errs, fmt.Errorf("%s: from is required", where))
		}
		for j, st := range s.Steps {
			if n := st.kinds(); n != 1 {
				errs = append(errs, fmt.Errorf("%s step %d: set exactly one instruction (found %d)", where, j+1, n))
			}
			if st.Copy != nil {
				if len(st.Copy.Src) == 0 || st.Copy.Dest == "" {
					errs = append(errs, fmt.Errorf("%s step %d: copy needs src and dest", where, j+1))
				}
				if st.Copy.From != "" && !seen[st.Copy.From] {
					errs = append(errs, fmt.Errorf("%s step %d: copy from unknown earlier stage %q", where, j+1, st.Copy.From))
				}
			}
		}
		if s.Name != "" {
			if seen[s.Name] {
				errs = append(errs, fmt.Errorf("duplicate stage name %q", s.Name))
			}
			seen[s.Name] = true
		}
	}
	return errors.Join(errs...)
}

func (s Step) kinds() int {
	n := 0
	for _, set := range []bool{
		s.Run != "", s.Copy != nil, len(s.Env) > 0, s.Workdir != "", len(s.Expose) > 0,
		len(s.Cmd) > 0, len(s.Entrypoint) > 0, s.User != "", len(s.Label) > 0,
	} {
		if set {
			n++
		}
	}
	return n
}

// ImageName returns the repository the built image is stored and pushed as.
func (p *Project) ImageName() string {
	if p.Image != "" {
		return p.Image
	}
	return p.Name
}

// ImageTags returns the tags to apply, defaulting to latest.
func (p *Project) ImageTags() []string {
	if len(p.Tags) == 0 {
		return []string{"latest"}
	}
	return p.Tags
}

// ContextDir returns the absolute build context directory.
func (p *Project) ContextDir() string {
	if p.Context == "" {
		return p.Dir
	}
	if filepath.IsAbs(p.Context) {
		return p.Context
	}
	return filepath.Join(p.Dir, p.Context)
}

// SortedKeys returns the keys of m in sorted order, so builds are reproducible.
func SortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
