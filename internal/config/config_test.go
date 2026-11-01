package config

import (
	"reflect"
	"strings"
	"testing"
)

const sample = `
name: web
image: ghcr.io/me/web
tags: [latest, v1]
stages:
  - name: build
    from: node:20-alpine
    steps:
      - workdir: /app
      - copy: { src: [package.json, package-lock.json], dest: ./ }
      - run: npm ci
  - from: nginx:1.27-alpine
    steps:
      - copy: { from: build, src: /app/dist, dest: /usr/share/nginx/html }
      - expose: "80"
      - cmd: nginx -g 'daemon off;'
run:
  ports: ["8080:80"]
  memory: 256m
  cpus: 0.5
`

func TestParseSample(t *testing.T) {
	p, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if p.ImageName() != "ghcr.io/me/web" || !reflect.DeepEqual(p.ImageTags(), []string{"latest", "v1"}) {
		t.Errorf("image = %s %v", p.ImageName(), p.ImageTags())
	}
	final := p.Stages[1]
	if final.Steps[0].Copy.From != "build" || final.Steps[0].Copy.Src[0] != "/app/dist" {
		t.Errorf("copy = %+v", final.Steps[0].Copy)
	}
	if !reflect.DeepEqual([]string(final.Steps[1].Expose), []string{"80"}) {
		t.Errorf("expose = %v", final.Steps[1].Expose)
	}
	if final.Steps[2].Cmd[0] != "/bin/sh" {
		t.Errorf("shell-form cmd = %v", final.Steps[2].Cmd)
	}
	if p.Run.CPUs != 0.5 || p.Run.Memory != "256m" {
		t.Errorf("run = %+v", p.Run)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]string{
		"no name":       "stages: [{from: alpine}]",
		"no stages":     "name: x",
		"two kinds":     "name: x\nstages: [{from: alpine, steps: [{run: a, workdir: /b}]}]",
		"unknown from":  "name: x\nstages: [{from: alpine, steps: [{copy: {from: nope, src: a, dest: b}}]}]",
		"unknown field": "name: x\nbogus: 1\nstages: [{from: alpine}]",
		"both":          "name: x\ndockerfile: Dockerfile\nstages: [{from: alpine}]",
	}
	for name, src := range cases {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s: empty error", name)
		}
	}
}
