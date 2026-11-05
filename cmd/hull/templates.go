package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const defaultIgnore = `# Files left out of the build context (same syntax as .dockerignore).
.git
node_modules
**/.DS_Store
*.log
.env
.env.*
hull.yaml
`

// nginxConf serves a single-page app on port 8080 (Cloud Run's default) and
// falls back to index.html so client-side routes survive a refresh.
const nginxConf = `server {
    listen 8080;
    server_name _;
    root /usr/share/nginx/html;
    index index.html;

    location / {
        try_files $uri $uri/ /index.html;
    }

    location ~* \.(?:js|css|svg|png|jpg|jpeg|gif|ico|woff2?)$ {
        expires 30d;
        add_header Cache-Control "public, immutable";
        try_files $uri =404;
    }

    gzip on;
    gzip_types text/plain text/css application/javascript application/json image/svg+xml;
}
`

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9._-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	if s == "" {
		return "app"
	}
	return s
}

type pkgJSON struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func (p pkgJSON) has(dep string) bool {
	_, a := p.Dependencies[dep]
	_, b := p.DevDependencies[dep]
	return a || b
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func initTemplate(dir, kind, name string) (string, string, error) {
	var pkg pkgJSON
	hasPkg := false
	if b, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		hasPkg = json.Unmarshal(b, &pkg) == nil
	}
	if kind == "auto" {
		switch {
		case exists(dir, "Dockerfile"):
			kind = "dockerfile"
		case hasPkg && (pkg.has("react") || pkg.has("vite") || pkg.has("react-scripts")):
			kind = "react"
		case hasPkg:
			kind = "node"
		case exists(dir, "index.html"):
			kind = "static"
		default:
			return "", "", fmt.Errorf("could not tell what kind of project this is; pass --template react, static, node or dockerfile")
		}
	}
	header := fmt.Sprintf(`# hull.yaml: how hull builds, runs and ships this project.
name: %s
# Where "hull push" sends the image, e.g. ghcr.io/you/%s or an ECR repository URL.
image: %s
tags: [latest]
# Most cloud services (ECS Fargate, Cloud Run) run linux/amd64.
platform: linux/amd64
`, name, name, name)

	switch kind {
	case "dockerfile":
		return header + `
dockerfile: Dockerfile

run:
  ports: ["8080"]
`, kind, nil
	case "react":
		out := "dist"
		if pkg.has("react-scripts") {
			out = "build"
		}
		lockSrc, install := installCommand(dir)
		if err := writeNginxConf(dir); err != nil {
			return "", "", err
		}
		return header + fmt.Sprintf(`
stages:
  # 1. Build the app with Node. RUN steps need Linux; on a Mac you can instead
  #    run "npm run build" yourself and use: hull init --template static
  - name: build
    from: node:20-alpine
    steps:
      - workdir: /app
      - copy: { src: %s, dest: ./ }
      - run: %s
      - copy: { src: ., dest: . }
      - run: npm run build

  # 2. Serve the built files with nginx on port 8080.
  - from: nginx:1.27-alpine
    steps:
      - copy: { src: nginx.conf, dest: /etc/nginx/conf.d/default.conf }
      - copy: { from: build, src: /app/%s, dest: /usr/share/nginx/html }
      - expose: "8080"
      - cmd: ["nginx", "-g", "daemon off;"]

run:
  ports: ["8080"]
  memory: 256m
`, lockSrc, install, out), kind, nil
	case "static":
		out := "dist"
		if !exists(dir, "dist") && exists(dir, "build") {
			out = "build"
		}
		if !exists(dir, out) && exists(dir, "index.html") {
			out = "."
		}
		if err := writeNginxConf(dir); err != nil {
			return "", "", err
		}
		return header + fmt.Sprintf(`
# Serves already-built static files, so it works on any OS (no RUN steps).
# Build your app first (e.g. npm run build), then: hull build
stages:
  - from: nginx:1.27-alpine
    steps:
      - copy: { src: nginx.conf, dest: /etc/nginx/conf.d/default.conf }
      - copy: { src: %s, dest: /usr/share/nginx/html }
      - expose: "8080"
      - cmd: ["nginx", "-g", "daemon off;"]

run:
  ports: ["8080"]
  memory: 128m
`, out), kind, nil
	case "node":
		lockSrc, install := installCommand(dir)
		install = strings.Replace(install, "npm ci", "npm ci --omit=dev", 1)
		return header + fmt.Sprintf(`
stages:
  - from: node:20-alpine
    steps:
      - workdir: /app
      - env: { NODE_ENV: production, PORT: "8080" }
      - copy: { src: %s, dest: ./ }
      - run: %s
      - copy: { src: ., dest: . }
      - user: node
      - expose: "8080"
      - cmd: ["npm", "start"]

run:
  ports: ["8080"]
  memory: 512m
`, lockSrc, install), kind, nil
	}
	return "", "", fmt.Errorf("unknown template %q (use auto, react, static, node or dockerfile)", kind)
}

func installCommand(dir string) (string, string) {
	switch {
	case exists(dir, "package-lock.json"):
		return "[package.json, package-lock.json]", "npm ci"
	case exists(dir, "yarn.lock"):
		return "[package.json, yarn.lock]", "yarn install --frozen-lockfile"
	case exists(dir, "pnpm-lock.yaml"):
		return "[package.json, pnpm-lock.yaml]", "corepack enable && pnpm install --frozen-lockfile"
	}
	return "[package.json]", "npm install"
}

func writeNginxConf(dir string) error {
	p := filepath.Join(dir, "nginx.conf")
	if exists(dir, "nginx.conf") {
		return nil
	}
	return os.WriteFile(p, []byte(nginxConf), 0o644)
}
