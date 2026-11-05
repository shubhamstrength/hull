# How existing container tools work

Before building our own tool, we looked at the container tools people already use, based on their public documentation and the open OCI specifications. This is what shaped the design.

| Tool | Builds images | Runs containers | Daemon | Image format | Push to registries |
| --- | --- | --- | --- | --- | --- |
| Docker | `Dockerfile` with BuildKit, layer cache | Through containerd, which calls runc | Yes (`dockerd`, root) | Docker v2 / OCI | Yes |
| Podman | `Containerfile` (via buildah) | Calls crun or runc directly | No, rootless by default | OCI | Yes |
| buildah | Containerfile, or one step at a time from a script | Only to run build steps | No | OCI | Yes |
| runc / crun | No | Runs an OCI bundle (`config.json` + rootfs) | No | Not applicable | No |
| ko / Jib / Cloud Native Buildpacks | Builds from the source tree with no Dockerfile | No | No | OCI | Yes |
| kaniko | Dockerfile, run in user space inside a container | No | No | OCI | Yes |

## How a container actually runs (Linux primitives)

- **Namespaces** isolate what a process can see. These are pid, mount, uts (hostname), ipc, net, user and cgroup. They are created with `clone(2)` or `unshare(2)` flags.
- **cgroups v2** limit resources. You make a directory under `/sys/fs/cgroup` and write to `memory.max`, `cpu.max` and `pids.max`, then add the process's PID to `cgroup.procs`.
- **Root filesystem.** The image layers are stacked read-only with **overlayfs**, and a writable upper layer goes on top. `pivot_root(2)` then makes that the process's `/`. This is safer than `chroot`.
- **Networking.** The simplest option is the host network. Docker's default creates a bridge (`docker0`), gives the container a veth pair in its own net namespace, and uses iptables NAT for published ports.
- **Hardening** covers dropping capabilities, a seccomp filter, no-new-privileges, and a read-only `/proc` and `/sys`. runc's `config.json` describes all of these.

## Image format (OCI image spec)

- An image is an **index**, which points to one **manifest** per platform. Each manifest points to a **config** blob and an ordered list of **layer** blobs.
- Each layer is a tar.gz of filesystem changes. A deleted file is recorded as a `.wh.<name>` whiteout file.
- Every blob is addressed by its `sha256` digest. That is what makes layers cacheable and shareable between images.
- The config blob holds the entrypoint, cmd, env, workdir, exposed ports and the history of layer digests (`diff_ids`).

## Registries (OCI distribution spec)

- Push works like this: `POST /v2/<name>/blobs/uploads/`, then `PUT` each blob with its `?digest=`, then `PUT /v2/<name>/manifests/<tag>`.
- Login uses a token flow: a `401` response includes a `WWW-Authenticate: Bearer realm=...` header, and the client gets a token from that realm. ECR, GCR/Artifact Registry, GHCR and Docker Hub all follow this.
- `HEAD` on a blob skips uploading layers the registry already has.

## Builds

- **Dockerfile steps.** `FROM` pulls the base. Each `RUN` executes in a container and becomes a new layer. `COPY` adds files as a layer. `ENV`, `WORKDIR`, `EXPOSE` and `CMD` only change the config.
- **Cache.** BuildKit keys each step on its parent layer plus the instruction, and for `COPY` on the hash of the copied files. Unchanged steps are reused.
- **Multi-stage builds** (`FROM ... AS build`, then `COPY --from=build`) keep the final image small. A React app needs this: build with Node, then serve the static `dist/` with a small web server.

## Deploying with Terraform

- Terraform doesn't build images. It references a pushed image by tag or, better, by digest.
- AWS ECS Fargate needs `aws_ecr_repository`, `aws_ecs_cluster`, `aws_ecs_task_definition` (with an image URI) and `aws_ecs_service`, plus a load balancer for public traffic.
- Google Cloud Run only needs `google_cloud_run_v2_service` with the image URI. It's the simplest target.

## What we take from this

- **No daemon.** Like Podman, the CLI does the work itself, which keeps it lightweight.
- **Standard OCI images.** Images must be OCI so Docker, Kubernetes, ECS and Cloud Run can run them unchanged. That is the main thing that makes Terraform deploys work.
- **Builds don't need a runtime for static steps.** `COPY`, `ENV` and `CMD` produce layers or config directly, so a React app can be built and pushed even on a Mac.
- **Per-project config.** A `hull.yaml` in each project holds the build steps, the run settings and the registry target, and an optional `Dockerfile` subset is accepted.
- **Run on Linux only** with namespaces, cgroups v2, overlayfs and pivot_root. On a Mac, running needs a Linux VM, the same as Docker Desktop.
- **Cache layers by digest** and skip pushing blobs the registry already has.
- **Terraform samples** for ECS Fargate and Cloud Run that take the pushed image digest as a variable.
