package dockerfile

import (
	"reflect"
	"strings"
	"testing"
)

const reactDockerfile = `
# syntax=docker/dockerfile:1
ARG NODE_VERSION=20
FROM node:${NODE_VERSION}-alpine AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
ENV NODE_ENV=production \
    VITE_API="https://api.example.com"
RUN npm run build

FROM nginx:1.27-alpine
LABEL org.opencontainers.image.source="https://github.com/me/app"
COPY --from=build /app/dist /usr/share/nginx/html
EXPOSE 80
HEALTHCHECK CMD wget -q -O- localhost
CMD ["nginx", "-g", "daemon off;"]
`

func TestParseReact(t *testing.T) {
	res, err := Parse(strings.NewReader(reactDockerfile), map[string]string{"NODE_VERSION": "22"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stages) != 2 {
		t.Fatalf("got %d stages, want 2", len(res.Stages))
	}
	b := res.Stages[0]
	if b.Name != "build" || b.From != "node:22-alpine" {
		t.Errorf("build stage = %q from %q", b.Name, b.From)
	}
	if got := b.Steps[1].Copy.Src; !reflect.DeepEqual([]string(got), []string{"package.json", "package-lock.json"}) {
		t.Errorf("copy src = %v", got)
	}
	if b.Steps[4].Env["VITE_API"] != "https://api.example.com" || b.Steps[4].Env["NODE_ENV"] != "production" {
		t.Errorf("env = %v", b.Steps[4].Env)
	}
	f := res.Stages[1]
	if f.Steps[1].Copy.From != "build" || f.Steps[1].Copy.Dest != "/usr/share/nginx/html" {
		t.Errorf("copy --from = %+v", f.Steps[1].Copy)
	}
	if !reflect.DeepEqual([]string(f.Steps[3].Cmd), []string{"nginx", "-g", "daemon off;"}) {
		t.Errorf("cmd = %v", f.Steps[3].Cmd)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "HEALTHCHECK") {
		t.Errorf("warnings = %v", res.Warnings)
	}
}

func TestParseErrors(t *testing.T) {
	for name, src := range map[string]string{
		"no from":       "RUN echo hi",
		"bad copy":      "FROM alpine\nCOPY onlyone",
		"unknown stage": "FROM alpine\nCOPY --from=nope /a /b",
		"remote add":    "FROM alpine\nADD https://example.com/x /x",
		"empty":         "# nothing",
	} {
		if _, err := Parse(strings.NewReader(src), nil); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestShellFormCmd(t *testing.T) {
	res, err := Parse(strings.NewReader("FROM alpine\nENV GREETING hello world\nCMD echo $GREETING"), nil)
	if err != nil {
		t.Fatal(err)
	}
	st := res.Stages[0].Steps
	if st[0].Env["GREETING"] != "hello world" {
		t.Errorf("legacy env = %v", st[0].Env)
	}
	if !reflect.DeepEqual([]string(st[1].Cmd), []string{"/bin/sh", "-c", "echo $GREETING"}) {
		t.Errorf("cmd = %v", st[1].Cmd)
	}
}
