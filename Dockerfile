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
    unzip \
    zip \
    ca-certificates \
    fontconfig \
    fonts-dejavu \
    libfreetype6 \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Install Java 21 JDK
RUN mkdir -p /etc/apt/keyrings && \
    wget -O /etc/apt/keyrings/adoptium.asc https://packages.adoptium.net/artifactory/api/gpg/key/public && \
    echo "deb [signed-by=/etc/apt/keyrings/adoptium.asc] https://packages.adoptium.net/artifactory/deb $(awk -F= '/^VERSION_CODENAME/{print$2}' /etc/os-release) main" | \
    tee /etc/apt/sources.list.d/adoptium.list && \
    apt-get update && \
    apt-get install -y temurin-21-jdk

# Install Gradle 8.7
RUN wget https://services.gradle.org/distributions/gradle-8.7-bin.zip -O /tmp/gradle.zip \
    && unzip -d /opt /tmp/gradle.zip \
    && rm /tmp/gradle.zip
ENV PATH="/opt/gradle-8.7/bin:${PATH}"

# Build Audiveris, pinned (LAL-546).
# bf57272e = upstream Audiveris tag 5.5.1 ("Merge development to master", 15 Apr 2025),
# the commit whose app/build.gradle sets project.version = '5.5.1'. Prod's image
# (nirmata1/audiforge@sha256:3233889..., pushed 17 Apr 2025 00:55 UTC) was built from the
# unpinned master of that day; every master commit between this one and the push touched
# only docs and workflows, so the engine code is identical.
ARG AUDIVERIS_COMMIT=bf57272e39ba267a49304d7f5a70f5633eb9e4d9
WORKDIR /app
RUN git clone https://github.com/Nirmata-1/audiveris.git && \
    git -C audiveris checkout --detach ${AUDIVERIS_COMMIT}
WORKDIR /app/audiveris
RUN ./gradlew build

# OCR language data (LAL-546).
# Audiveris 5.5.1 runs Tesseract 5.3.1 with OEM_TESSERACT_ONLY (the legacy engine), so it
# needs the tesseract-ocr/tessdata models (legacy + LSTM), NOT tessdata_fast or
# tessdata_best (LSTM only). Tag 4.1.0 is the one Audiveris itself pins
# (gradle.properties: theTessdataTag = 4.1.0); 4767ea92 is that tag's commit.
# The engine reads TESSDATA_PREFIX first (TesseractOCR.findOcrFolder). main.go keeps it
# only when AUDIVERIS_OCR_LANGS is set, and strips it otherwise, so with the variable
# empty the engine falls back to its empty config tessdata folder, exactly as prod today.
ARG TESSDATA_COMMIT=4767ea922bcc460e70b87b1d303ebdfed0897da8
ENV TESSDATA_PREFIX=/usr/share/audiveris/tessdata
RUN mkdir -p ${TESSDATA_PREFIX} && cd ${TESSDATA_PREFIX} && \
    for l in eng deu fra ita spa nld lat heb; do \
      wget --no-verbose "https://github.com/tesseract-ocr/tessdata/raw/${TESSDATA_COMMIT}/${l}.traineddata" || exit 1; \
    done && \
    printf '%s\n' \
      "daa0c97d651c19fba3b25e81317cd697e9908c8208090c94c3905381c23fc047  eng.traineddata" \
      "896b3b4956503ab9daa10285db330881b2d74b70d889b79262cc534b9ec699a4  deu.traineddata" \
      "eac01c1d72540d6090facb7b2f42dd0a2ee8fc57c5be1b20548ae668e2761913  fra.traineddata" \
      "4f7476c611312beb8f8e182888da08ea642d9824ae4402cc6235f61ab1406406  ita.traineddata" \
      "0b0fcbb4665189e01ab8019e591f014dd7260460de072543edd4b2cb4ede7c96  spa.traineddata" \
      "b1f6767f50daf5903802fb970e7b9f64ee1f12be0b2b324dfdf8e8b05771572f  nld.traineddata" \
      "790fc854bc28655aa70a776e3e014548bd42e8811847b84a61c9e53ef0b0ea2c  lat.traineddata" \
      "7da6ea6b7a2620ec8e8b41de2967a13d429635a56657a0b30b622501a573d3e1  heb.traineddata" \
    | sha256sum -c - && \
    chmod 644 ${TESSDATA_PREFIX}/*.traineddata

# Copy Go artifacts from first stage
COPY --from=go-builder /app/audiforge /app/
COPY --from=go-builder /app/templates /app/templates

# Setup environment
RUN mkdir -p /tmp/uploads /tmp/downloads && \
    chmod -R 755 /tmp/uploads /tmp/downloads /app/templates

ENV AUDIVERIS_HOME=/app/audiveris \
    UPLOAD_DIR=/tmp/uploads \
    DOWNLOAD_DIR=/tmp/downloads \
    AUDIVERIS_OCR_LANGS="" \
    LOG=""

EXPOSE 8080
ENTRYPOINT ["/app/audiforge"]
