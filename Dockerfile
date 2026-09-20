# Stage 1: Build Go application
FROM golang:1.24 AS go-builder

WORKDIR /app
COPY . .
COPY templates/ ./templates/
RUN go build -o audiforge .

# Stage 2: Build Audiveris and final image
FROM debian:bookworm-slim

# Install system dependencies
RUN apt-get update && \
    apt-get install -y \
    git \
    wget \
    curl \
    unzip \
    zip \
    ca-certificates \
    fontconfig \
    fonts-dejavu \
    libfreetype6 \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Install Java 25 JDK. Audiveris master (gradle.properties theMinJavaVersion)
# moved to 25; the old image was built when 21 was enough, and a rebuild
# with 21 fails at :app:compileJava ("invalid source release: 25").
RUN mkdir -p /etc/apt/keyrings && \
    wget -O /etc/apt/keyrings/adoptium.asc https://packages.adoptium.net/artifactory/api/gpg/key/public && \
    echo "deb [signed-by=/etc/apt/keyrings/adoptium.asc] https://packages.adoptium.net/artifactory/deb $(awk -F= '/^VERSION_CODENAME/{print$2}' /etc/os-release) main" | \
    tee /etc/apt/sources.list.d/adoptium.list && \
    apt-get update && \
    apt-get install -y temurin-25-jdk

# Install Gradle 8.7
RUN wget https://services.gradle.org/distributions/gradle-8.7-bin.zip -O /tmp/gradle.zip \
    && unzip -d /opt /tmp/gradle.zip \
    && rm /tmp/gradle.zip
ENV PATH="/opt/gradle-8.7/bin:${PATH}"

# The Gradle daemon must never run in this image. It is a long-lived
# background JVM that holds the build-output-cleanup lock, and a request-time
# Gradle invocation that hangs takes every later request down with it
# ("Timeout waiting to lock Build Output Cleanup Cache", 2026-09-18 outage).
ENV GRADLE_OPTS="-Dorg.gradle.daemon=false"

# Build Audiveris
WORKDIR /app
# Pinned: an unpinned clone means every rebuild picks up whatever Audiveris
# master is that day (this is how the Java requirement silently moved).
# Bump AUDIVERIS_REF deliberately, with a build check.
ARG AUDIVERIS_REF=bdf8a439
RUN git clone https://github.com/Nirmata-1/audiveris.git && \
    cd audiveris && git checkout --quiet "${AUDIVERIS_REF}"
WORKDIR /app/audiveris
# No `gradlew build` here: it runs Audiveris's own test suite, and on a
# headless builder one of those tests waits forever on libgtk (the 2026-09-20
# CI run sat in GlyphFactoryTest for an hour). installDist below compiles
# everything the launcher needs and runs no tests.

# Install a runnable distribution. The ':app' sub-project applies the Gradle
# 'application' plugin, so installDist writes a self-contained tree with a
# start script under app/build/install/<dist>/bin/. The dist directory name
# depends on the application plugin's applicationName, so it is discovered
# here rather than hard-coded, and the build FAILS LOUDLY if it is absent:
# a deploy that silently falls back to `gradle run` per request is the bug
# this whole change exists to remove.
RUN ./gradlew --no-daemon :app:installDist
RUN set -eux; \
    launcher="$(find /app/audiveris -path '*/build/install/*/bin/*' -type f ! -name '*.bat' | head -n1)"; \
    if [ -z "$launcher" ]; then \
        echo "FATAL: installDist produced no launcher under /app/audiveris" >&2; \
        find /app/audiveris -path '*/build/install/*' -maxdepth 6 >&2 || true; \
        exit 1; \
    fi; \
    echo "Audiveris launcher: $launcher"; \
    chmod +x "$launcher"; \
    printf '#!/bin/sh\nexec "%s" "$@"\n' "$launcher" > /usr/local/bin/audiveris; \
    chmod +x /usr/local/bin/audiveris

# No Gradle process runs after this point, so no lock file in the image is
# ever live. Drop the ones the build left behind.
RUN find /app/audiveris/.gradle /root/.gradle -name '*.lock' -delete 2>/dev/null || true

# Copy Go artifacts from first stage
COPY --from=go-builder /app/audiforge /app/
COPY --from=go-builder /app/templates /app/templates

# Setup environment
RUN mkdir -p /tmp/uploads /tmp/downloads && \
    chmod -R 755 /tmp/uploads /tmp/downloads /app/templates

ENV AUDIVERIS_HOME=/app/audiveris \
    AUDIVERIS_LAUNCHER=/usr/local/bin/audiveris \
    AUDIVERIS_JVM_OPTS=-Xmx3g \
    AUDIVERIS_TIMEOUT_SECONDS=330 \
    MAX_CONCURRENT_JOBS=1 \
    MAX_PDF_PAGES=40 \
    UPLOAD_DIR=/tmp/uploads \
    DOWNLOAD_DIR=/tmp/downloads \
    LOG=""

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/app/audiforge"]
