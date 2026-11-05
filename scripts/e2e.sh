#!/usr/bin/env bash
# End-to-end check on Linux, as root: build an app image, run it, serve HTTP,
# use the dashboard API, and clean up. Usage: sudo scripts/e2e.sh ./hull
set -euo pipefail

HULL=$(realpath "${1:-./hull}")
export HULL_HOME=$(mktemp -d)
WORK=$(mktemp -d)
trap '"$HULL" rm -f e2e-web e2e-ui >/dev/null 2>&1 || true; kill $UI_PID 2>/dev/null || true; rm -rf "$HULL_HOME" "$WORK"' EXIT
UI_PID=

step() { printf '\n== %s\n' "$*"; }

step "run a one-off container"
out=$("$HULL" run --rm alpine:3.20 sh -c 'echo pid=$$ host=$(hostname)')
echo "$out"
grep -q 'pid=1 ' <<<"$out"

step "exit codes are passed through"
set +e; "$HULL" run --rm alpine:3.20 sh -c 'exit 7'; code=$?; set -e
[ "$code" = 7 ]

step "build a multi-stage app with RUN steps"
mkdir -p "$WORK/app/src"
cd "$WORK/app"
cat > package.json <<'JSON'
{ "name": "e2e", "version": "1.0.0", "scripts": { "build": "node build.js" } }
JSON
cat > package-lock.json <<'JSON'
{ "name": "e2e", "version": "1.0.0", "lockfileVersion": 3, "requires": true, "packages": { "": { "name": "e2e", "version": "1.0.0" } } }
JSON
cat > build.js <<'JS'
const fs = require("fs");
fs.mkdirSync("dist", { recursive: true });
fs.writeFileSync("dist/index.html", "<h1>hull e2e</h1>\n");
JS
"$HULL" init --template react --name e2e-web
"$HULL" build
"$HULL" build > "$WORK/build2.log"
grep -q CACHED "$WORK/build2.log"

step "run it in the background and fetch a page"
"$HULL" run -d
for i in $(seq 1 50); do curl -fsS localhost:8080/ >/dev/null 2>&1 && break; sleep 0.2; done
curl -fsS localhost:8080/ | grep -q 'hull e2e'
curl -fsS localhost:8080/deep/link | grep -q 'hull e2e'   # SPA fallback
"$HULL" ps > "$WORK/ps.log"; grep -q e2e-web "$WORK/ps.log"

step "dashboard API"
"$HULL" ui --addr 127.0.0.1:7071 & UI_PID=$!
for i in $(seq 1 50); do curl -fsS localhost:7071/api/info >/dev/null 2>&1 && break; sleep 0.2; done
curl -fsS localhost:7071/api/containers | grep -q '"status":"running"'
curl -fsS -X POST -H 'X-Hull: 1' localhost:7071/api/containers/e2e-web/stop
"$HULL" ps -a > "$WORK/ps.log"; grep e2e-web "$WORK/ps.log" | grep -q Exited

step "save the image as a docker-loadable tarball"
"$HULL" save -o "$WORK/img.tar" e2e-web:latest
tar tf "$WORK/img.tar" > "$WORK/tar.log"; grep -q manifest.json "$WORK/tar.log"

step "clean up"
"$HULL" rm e2e-web
"$HULL" rmi e2e-web:latest

echo
echo "e2e passed"
