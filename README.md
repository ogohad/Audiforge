# Audiforge - PDF to MusicXML Conversion

A **Go-based web service** that leverages **Audiveris** for converting PDF sheet music to MusicXML format.

---

## Table of Contents

- [Docker Installation](#docker-installation)
- [Local Development Setup](#local-development-setup)
- [Environment Variables](#environment-variables)
- [Configuration](#configuration)
- [API Endpoints](#api-endpoints)
- [LalaScript hardening](#lalascript-hardening)
- [Project Structure](#project-structure)
- [Troubleshooting](#troubleshooting)
- [FAQ](#faq)
- [Contributing](#contributing)
- [License](#license)

---

## Docker Installation

### Prerequisites

- Docker installed ([installation guide](https://docs.docker.com/get-docker/))

### Steps

1. **Pull the Docker image:**

   ```bash
   docker pull nirmata1/audiforge:latest
   ```

2. **Run the container:**

   ```bash
   docker run -d -p 8080:8080 \
     -e LOG=debug #optional
     -v /path/to/uploads:/tmp/uploads \
     -v /path/to/downloads:/tmp/downloads \
     nirmata1/audiforge:latest
   ```   
---

## Local Development Setup

### Requirements

- Go 1.20+
- Java 17+ (for Audiveris)
- Gradle 7+

### Installation Guide

#### Clone Repositories

```bash
# Create project directory
mkdir audiforge && cd audiforge

# Clone Audiveris
git clone https://github.com/Audiveris/audiveris.git

# Build Audiveris
cd audiveris
./gradlew build
```

#### Set Up Go Application

```bash
cd ..
git clone https://github.com/Nirmata-1/Audiforge.git
cd audiforge-go
go build
```

#### Run the Service

```bash
LOG=debug ./audiforge
```

---

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LOG`    | info    | Set to `debug` to show Audiveris logs in console |

---

## Configuration

### Key Directories

| Directory      | Path             | Purpose                      |
|----------------|------------------|------------------------------|
| Uploads        | `/tmp/uploads`   | Temporary PDF storage        |
| Downloads      | `/tmp/downloads` | Converted MusicXML files     |
| Audiveris Home | `./audiveris`    | Audiveris engine installation |

### Cleanup Process

- Automatic cleanup runs every hour
- Files older than 1 hour are deleted
- Executed via background goroutine

---

## API Endpoints

| Method | Endpoint         | Description                    |
|--------|------------------|--------------------------------|
| POST   | `/upload`        | Upload PDF file                |
| GET    | `/status/{id}`   | Check conversion status        |
| GET    | `/download/{id}` | Download ZIP of MusicXML files |
| GET    | `/health`        | Liveness + queue depth (see [LalaScript hardening](#lalascript-hardening)) |

---

## Project Structure

```
audiforge/
├── audiveris/          # Audiveris engine
│   ├── build/
│   ├── app/
│   └── gradlew
├── main.go             # Go application
├── go.mod
├── go.sum
├── templates/          # Web UI
└── Dockerfile
```

---

---

## LalaScript hardening

This fork is the copy LalaScript runs on Railway. The changes below all come
out of one incident and are meant to make it impossible to repeat.

### What happened (2026-09-18)

A 286-page PDF was uploaded. The service ran `gradle run` inside
`/app/audiveris` for that request, as it did for every request. The Gradle
invocation hung. The client gave up after six minutes; the Gradle process did
not, and kept running for **34 hours** holding
`/app/audiveris/.gradle/buildOutputCleanup/buildOutputCleanup.lock`. Every
later upload then died after about a minute with

```
Timeout waiting to lock Build Output Cleanup Cache ... It is currently in use by another Gradle instance.
```

which reached the client as
`{'status':'error','message':'Conversion failed - no movements generated (exec error: exit status 1)'}`.
One oversized PDF took the whole service down until someone noticed.

### The four fixes

1. **No Gradle at request time.** The Dockerfile now runs
   `./gradlew --no-daemon :app:installDist` at build time and writes a small
   wrapper at `/usr/local/bin/audiveris` pointing at the generated start
   script. At request time the service execs that launcher directly with
   `-batch -export -output <dir> -- <pdf>`. No build system, no lock file, no
   daemon. `GRADLE_OPTS=-Dorg.gradle.daemon=false` is baked into the image and
   any remaining Gradle call passes `--no-daemon`. The old
   `gradle run` path survives only as a fallback for local development when
   the launcher is missing; the log line `engine lane = installDist` or
   `engine lane = gradle-fallback` says which one ran, and the image build
   fails outright if `installDist` produced no launcher.
2. **A hard per-job timeout.** The conversion runs under
   `exec.CommandContext` in its own process group (`Setpgid`). On deadline the
   entire group gets `SIGKILL`, so the JVM cannot outlive the request. Nothing
   is left holding anything.
3. **One conversion at a time.** A buffered-channel semaphore admits
   `MAX_CONCURRENT_JOBS` jobs (default 1). A second upload reports status
   `queued` instead of starting a second `-Xmx3g` JVM on the same container.
   `queued` is a *processing* state: the LalaScript client stops only on
   `done`/`complete`/`completed`/`finished` or `error`/`failed`, and the
   browser UI keeps polling on anything it does not recognise.
4. **A page cap.** `/upload` counts page objects with a cheap byte scan (no
   PDF library) and refuses anything over `MAX_PDF_PAGES` with HTTP 413 and a
   JSON body. The scan fails open: if it cannot tell (compressed object
   streams, encryption), the job is allowed through and the timeout is the
   backstop.

### Also

- **`GET /health`** returns
  `{"status":"ok","busy":n,"queued":m,"last_success_unix":t,"last_error":"..."}`
  for a Railway healthcheck or an external probe. A `HEALTHCHECK` in the
  Dockerfile uses it.
- **Startup hygiene.** On boot the service deletes any `*.lock` under
  `$AUDIVERIS_HOME/.gradle` and `/root/.gradle` (nothing holds them at boot,
  so this is always safe and cures exactly the 2026-09-18 wedge if it ever
  arrives from a restored volume), and sweeps upload/download entries older
  than 24 hours.
- **Periodic cleanup actually runs.** It used to return immediately unless
  `LOG=debug`, so the production container never cleaned up anything.
- `UPLOAD_DIR` and `DOWNLOAD_DIR` are now honoured (they were declared in the
  Dockerfile but hard-coded in `main.go`).
- `/status/{id}` and `/download/{id}` reject ids that are not plain
  `[A-Za-z0-9_-]`, so the path cannot be walked out of the download directory.

The HTTP contract is unchanged: `POST /upload` -> `{"id": ...}`,
`GET /status/{id}`, `GET /download/{id}` -> ZIP.

### Environment variables

| Variable | Default | Why |
|----------|---------|-----|
| `AUDIVERIS_LAUNCHER` | `/usr/local/bin/audiveris` | The `installDist` start script. If this file is missing or not executable, the service logs a warning and falls back to `gradle --no-daemon run`. |
| `AUDIVERIS_JVM_OPTS` | `-Xmx3g` | Passed to the launcher via `JAVA_OPTS`. The Gradle-generated start script concatenates `DEFAULT_JVM_OPTS`, `JAVA_OPTS` and `AUDIVERIS_OPTS` onto the `java` command line, so either of the latter two works; `JAVA_OPTS` is used because it does not depend on the application name. On the fallback path the same value goes to `-PjvmLineArgs`. |
| `AUDIVERIS_TIMEOUT_SECONDS` | `330` | Wall-clock ceiling for one conversion. LalaScript's client polls for 360 s, so killing at 330 s guarantees the client reads a clean `error` status instead of timing out on its own side with the job still running. |
| `MAX_CONCURRENT_JOBS` | `1` | Concurrent Audiveris processes per container. Two `-Xmx3g` JVMs on one Railway box OOM each other. |
| `MAX_PDF_PAGES` | `40` | Upload-time page cap; over it, HTTP 413. Set to `0` to disable. |
| `AUDIVERIS_HOME` | `/app/audiveris` | Audiveris checkout. Used by the Gradle fallback and by the boot-time lock sweep. |
| `UPLOAD_DIR` | `/tmp/uploads` | Where uploaded PDFs land. |
| `DOWNLOAD_DIR` | `/tmp/downloads` | Where exported MusicXML lands, one sub-directory per job. |
| `LOG` | *(empty)* | `debug` mirrors the Audiveris log to stdout. |

### Job statuses

| Status | Meaning | Client behaviour |
|--------|---------|------------------|
| `queued` | Uploaded, waiting for a conversion slot | keep polling |
| `processing` | Audiveris is running | keep polling |
| `completed` | At least one `.mxl` exported | download |
| `error` | Nothing exported, or killed on the timeout | fail the job |


## Troubleshooting

### Common Issues

**Missing Dependencies**

```bash
# Install Java & Gradle
sudo apt install openjdk-17-jdk gradle
```

**Permission Denied**

```bash
chmod +x audiveris/gradlew
```

**Gradle Build Failure**

```bash
cd audiveris && ./gradlew clean build
```

**Missing Files After Conversion**

- Check `/tmp` permissions
- Verify available disk space

---

## FAQ

**Q: Can it handle multi-movement scores?**  
A: The service automatically detects movements and packages them in a ZIP file.

**Q: Can I change the cleanup interval?**  
A: Modify `CleanupInterval` in `main.go` and rebuild.

**Q: How to enable debug logging?**  
A: Run with `LOG=debug ./audiforge`

---

## Contributing

1. Fork the repository  
2. Create a feature branch  
3. Submit a PR with tests  
4. Follow better coding standards than me

---

## License

**Apache 2.0 © 2025 Jermiah Jeffries**
