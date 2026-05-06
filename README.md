# Volust

Volust is a Docker volume backup orchestrator written in Go.

Applications opt in with `volust.*` labels. Volust reads labeled running containers through the Docker socket by default, resolves mounted backup sources, and starts one-shot restic job containers for backup, retention cleanup, pruning, and destructive restore. Stopped containers can be included with an explicit environment switch.

Repository: `github.com/monlor/volust`

## Quick Start

Quick start uses S3 only and does not require a Volust config file.

Set the required S3 environment variables:

```bash
export VOLUST_S3_REPOSITORY=s3:s3.amazonaws.com/my-bucket/volust
export RESTIC_PASSWORD='change-me'
export AWS_ACCESS_KEY_ID='change-me'
export AWS_SECRET_ACCESS_KEY='change-me'
export AWS_DEFAULT_REGION='us-east-1'
```

Start Volust with the minimal Docker command:

```bash
docker run -d \
  --name volust \
  --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -e TZ=Asia/Shanghai \
  -e VOLUST_S3_REPOSITORY="$VOLUST_S3_REPOSITORY" \
  -e RESTIC_PASSWORD="$RESTIC_PASSWORD" \
  -e AWS_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
  -e AWS_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
  -e AWS_DEFAULT_REGION="$AWS_DEFAULT_REGION" \
  -e 'VOLUST_DEFAULT_SCHEDULE=0 3 * * *' \
  -e VOLUST_DEFAULT_RETENTION=keep-last=7,keep-daily=7,keep-weekly=4,keep-monthly=6 \
  -e VOLUST_MAX_CONCURRENT_WRITES=4 \
  ghcr.io/monlor/volust:latest
```

The image starts `volust daemon` by default. You do not need to set `--entrypoint`, pass `daemon`, or run `restic init` yourself. Volust initializes the repository automatically before the first backup.

This starts one default profile named `default`. Application containers can omit `volust.profile` unless they need a non-default profile.

Add labels to an application container:

```bash
docker run -d \
  --name my-app \
  -v my-app-data:/data \
  --label volust.enabled=true \
  alpine:3.20 sleep infinity
```

With the global defaults above, the application only needs `volust.enabled=true`. Volust backs up all regular bind and volume mounts by default, excluding socket and device/system mounts such as `/var/run/docker.sock`, `/dev`, `/proc`, and `/sys`.

Run one immediate backup scan using the existing Volust container:

```bash
docker exec volust volust daemon --once
```

List discovered backup applications and sources:

```bash
docker exec volust volust apps
docker exec volust volust apps --profile default
```

Trigger a backup for an application immediately:

```bash
docker exec -it volust volust backup
docker exec volust volust backup --profile default --app my-app
docker exec volust volust backup --profile default --app my-app --source data
```

When `--source` is omitted, Volust backs up all sources for the selected application. The command uses the same backup, retention, prune, exclude, and source-locking path as scheduled backups.

List snapshots for an application interactively:

```bash
docker exec -it volust volust snapshots
```

Or pass the selection directly:

```bash
docker exec volust volust snapshots --profile default --app my-app --source data
docker exec volust volust snapshots --profile default --app my-app --source data --snapshot latest
```

Restore interactively using the existing Volust container:

```bash
docker exec -it volust volust restore
```

Volust lists discovered applications and sources, then asks you to confirm the destructive restore.

You can also pass parameters directly:

```bash
docker exec -it volust volust restore --profile default --app my-app --source data --snapshot latest
```

Restore is destructive. Volust prints a confirmation phrase and will not start the restore job until you type it exactly. By default, restore first stops the application container only if it is currently running, runs a safety backup for the selected source, restores the selected snapshot, and then restores the container to its previous running state. A container that was stopped before restore stays stopped. Use `--skip-pre-backup` only when you explicitly do not want the safety backup.

## Docker Compose

The compose examples use the online image and environment variables only.

S3:

```bash
cd deploy
vi compose.s3.yaml
docker compose -f compose.s3.yaml up -d
```

WebDAV:

```bash
cd deploy
vi compose.webdav.yaml
docker compose -f compose.webdav.yaml up -d
```

Manual scan and restore are the same for compose deployments:

```bash
docker exec volust volust daemon --once
docker exec -it volust volust restore
```

## Environment Configuration

Volust defaults to one profile named `default`.

S3 variables:

- `VOLUST_S3_REPOSITORY`: restic S3 repository, for example `s3:s3.amazonaws.com/my-bucket/volust`
- `RESTIC_PASSWORD`: restic repository password
- `AWS_ACCESS_KEY_ID`: S3 access key
- `AWS_SECRET_ACCESS_KEY`: S3 secret key
- `AWS_DEFAULT_REGION`: S3 region

WebDAV variables:

- `VOLUST_PROFILE_TYPE=webdav`
- `VOLUST_WEBDAV_PATH`: repository path inside the WebDAV remote
- `VOLUST_WEBDAV_URL`: WebDAV endpoint
- `VOLUST_WEBDAV_USER`: WebDAV username
- `VOLUST_WEBDAV_PASS`: WebDAV password
- `VOLUST_WEBDAV_VENDOR`: optional rclone vendor, usually `other`
- `RESTIC_PASSWORD`: restic repository password

Shared variables:

- `VOLUST_PROFILE`: optional profile name, defaults to `default`
- `VOLUST_DEFAULT_SCHEDULE`: default five-field cron schedule for labeled containers that omit `volust.schedule`
- `VOLUST_DEFAULT_RETENTION`: default retention policy for labeled containers that omit `volust.retention`
- `VOLUST_JOB_IMAGE`: optional image for one-shot job containers, defaults to `ghcr.io/monlor/volust:latest`
- `VOLUST_INCLUDE_STOPPED_CONTAINERS`: include stopped labeled containers in backup/restore discovery when set to `true`, `1`, `yes`, or `on`; defaults to `false`
- `VOLUST_STOP_CONTAINERS_BEFORE_BACKUP`: stop running application containers while their backup job runs when set to `true`, `1`, `yes`, or `on`; defaults to `false`
- `VOLUST_MAX_CONCURRENT_WRITES`: maximum concurrent restic write jobs across repositories on the same Volust host/container, defaults to `4`; set to `0` to disable the global limit. Backup, forget, prune, and restore consume write slots. Snapshot listing stays read-only and does not consume a slot.

Jobs that use the same restic repository are still queued and run one at a time, while different repositories can run concurrently up to `VOLUST_MAX_CONCURRENT_WRITES`. Manual commands started with `docker exec volust volust backup` or `docker exec volust volust restore` share the same local write slots with daemon jobs. The global limit does not remove stale restic locks; clear stale backend locks with `restic unlock` after confirming no backup is running.

The compose files keep all variables inline so deployment is a single file. Edit the placeholder values before starting.

## Label Schema

- `volust.enabled=true`
- `volust.exclude=cache/**,tmp/**`
- `volust.exclude-file=media.txt,common.txt`

Optional labels:

- `volust.name=<app-name>` defaults to the Docker container name
- `volust.profile=<profile-name>` defaults to `default`
- `volust.sources=/data,/config` defaults to all regular bind and volume mounts, excluding socket and device/system mounts
- `volust.schedule=0 3 * * *` defaults to `VOLUST_DEFAULT_SCHEDULE`
- `volust.retention=keep-last=7,keep-daily=7,keep-weekly=4,keep-monthly=6` defaults to `VOLUST_DEFAULT_RETENTION`
- `volust.stop-before-backup=true|false` overrides `VOLUST_STOP_CONTAINERS_BEFORE_BACKUP` for that application

`volust.enabled=true` only opts a container into discovery. Backup frequency is controlled by `volust.schedule`; if that label is omitted, Volust uses `VOLUST_DEFAULT_SCHEDULE` from the Volust service environment. Stopping a container during backup is disabled by default; enable it globally with `VOLUST_STOP_CONTAINERS_BEFORE_BACKUP=true`, or override individual applications with `volust.stop-before-backup`. When enabled, Volust stops the running application container before its backup job starts and automatically starts it again after the backup job exits, including when the backup job fails. Restart cleanup uses a hard 2 minute timeout; if backup and restart both fail, both errors are returned or logged by the command/scheduler and the container may require manual intervention. The stop call uses the active command or scheduler context. The application is unavailable for the full stop, backup job, and restart window, so use this mode only when that downtime is acceptable or after putting the application into maintenance mode. The daemon refreshes discovered containers every minute and schedules each source with its cron expression. Jobs that use the same restic repository are queued and run one at a time, while different repositories can run concurrently. Use `docker exec volust volust backup --app <app-name>` for an immediate application backup outside the schedule, or `docker exec volust volust daemon --once` to scan and back up all discovered applications once.

When provided, sources must exactly match mounted paths inside the application container. Volust maps each source into a job container at `/volust/sources/<source-id>`.

Exclude files are read by the Volust daemon from `/etc/volust/excludes`. Mount that directory into the Volust container when using `volust.exclude-file`; the one-shot backup job does not need a separate mount.

## Local Development Build

Published deployments should use `ghcr.io/monlor/volust:latest`.

Local builds should use a `.dev` tag suffix:

```bash
docker build -t ghcr.io/monlor/volust:latest.dev .
```

Use the dev image for local tests:

```bash
docker run --rm ghcr.io/monlor/volust:latest.dev
```

## Release Images

GitHub Actions builds and publishes `ghcr.io/monlor/volust` for both `linux/amd64` and `linux/arm64`.

The workflow uses native runners for each architecture:

- `ubuntu-24.04` for `linux/amd64`
- `ubuntu-24.04-arm` for `linux/arm64`

Pushes to `main` publish `latest`. Tags like `v1.0.0` publish `v1.0.0` and `1.0.0`.
