package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Configuration
//
// Everything below is overridable from the environment so the same image can
// be tuned on Railway without a rebuild. See README "LalaScript hardening".
// ---------------------------------------------------------------------------

const (
	CleanupInterval = 15 * time.Minute
	FileTTL         = 1 * time.Hour

	// Entries older than this are swept once at boot, so a container that was
	// killed mid-job does not come back with a disk full of dead work.
	BootSweepTTL = 24 * time.Hour

	// Grace period between "kill the process group" and "give up waiting for
	// the I/O pipes to drain".
	killGrace = 10 * time.Second
)

var (
	UploadDir   = envStr("UPLOAD_DIR", "/tmp/uploads")
	DownloadDir = envStr("DOWNLOAD_DIR", "/tmp/downloads")

	// AudiverisHome is only used by the Gradle fallback path.
	AudiverisHome = envStr("AUDIVERIS_HOME", "/app/audiveris")

	// AudiverisLauncher is the installDist launcher baked into the image by
	// the Dockerfile. When it is present we exec it directly: no Gradle, no
	// Gradle daemon, no build-output-cleanup lock.
	AudiverisLauncher = envStr("AUDIVERIS_LAUNCHER", "/usr/local/bin/audiveris")

	// JVM options handed to the launcher through JAVA_OPTS (the Gradle
	// start-script template reads DEFAULT_JVM_OPTS, JAVA_OPTS and
	// AUDIVERIS_OPTS and concatenates all three).
	AudiverisJVMOpts = envStr("AUDIVERIS_JVM_OPTS", "-Xmx3g")

	// Hard wall-clock ceiling for one conversion. LalaScript's client
	// (jobs.py _audiforge_transcribe_pdf) polls for 360 s; we kill at 330 s
	// so the client always reads a clean {"status":"error"} instead of
	// hitting its own timeout and leaving a JVM running behind us.
	JobTimeout = time.Duration(envInt("AUDIVERIS_TIMEOUT_SECONDS", 330)) * time.Second

	// One conversion at a time per container by default: two -Xmx3g JVMs on
	// one Railway box is how you OOM both of them.
	MaxConcurrentJobs = envInt("MAX_CONCURRENT_JOBS", 1)

	// Refuse absurd page counts at upload time rather than discovering them
	// 34 hours later. 0 disables the cap.
	MaxPDFPages = envInt("MAX_PDF_PAGES", 40)
)

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

type ProcessingStatus struct {
	Status        string `json:"status"`
	Message       string `json:"message"`
	Timestamp     int64  `json:"timestamp"`
	MovementCount int    `json:"movementCount,omitempty"`
}

type healthReport struct {
	Status          string `json:"status"`
	Busy            int64  `json:"busy"`
	Queued          int64  `json:"queued"`
	LastSuccessUnix int64  `json:"last_success_unix"`
	LastError       string `json:"last_error"`
}

var (
	processing sync.Map
	templates  *template.Template

	// jobSlots is the concurrency semaphore. A job holds one slot for the
	// whole lifetime of the Audiveris process.
	jobSlots chan struct{}

	busyCount       atomic.Int64
	queuedCount     atomic.Int64
	lastSuccessUnix atomic.Int64

	lastErrMu sync.Mutex
	lastErr   string

	// idPattern keeps /status/ and /download/ from being talked into
	// traversing out of the download directory.
	idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

	// pageObjPattern matches a PDF page object dictionary entry. "/Type /Pages"
	// is the page-tree node, not a page, so the trailing character must not be
	// an 's'.
	pageObjPattern = regexp.MustCompile(`/Type\s*/Page(?:[^s]|$)`)
)

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("config: %s=%q is not a number, using default %d", key, raw, def)
		return def
	}
	return v
}

func recordError(msg string) {
	lastErrMu.Lock()
	lastErr = msg
	lastErrMu.Unlock()
}

func readLastError() string {
	lastErrMu.Lock()
	defer lastErrMu.Unlock()
	return lastErr
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

func main() {
	if MaxConcurrentJobs < 1 {
		log.Printf("config: MAX_CONCURRENT_JOBS must be >= 1, forcing 1")
		MaxConcurrentJobs = 1
	}
	jobSlots = make(chan struct{}, MaxConcurrentJobs)

	if err := os.MkdirAll(UploadDir, 0o755); err != nil {
		log.Printf("boot: cannot create upload dir %s: %v", UploadDir, err)
	}
	if err := os.MkdirAll(DownloadDir, 0o755); err != nil {
		log.Printf("boot: cannot create download dir %s: %v", DownloadDir, err)
	}

	if t, err := template.ParseGlob("/app/templates/*.html"); err != nil {
		// The browser UI is a convenience; the JSON API is the product. A
		// missing template directory must not stop the service from booting.
		log.Printf("boot: templates unavailable: %v", err)
	} else {
		templates = t
	}

	removeStaleGradleLocks()
	bootSweep()

	if launcherAvailable() {
		log.Printf("boot: Audiveris launcher %s found - Gradle will NOT run at request time", AudiverisLauncher)
	} else {
		log.Printf("boot: WARNING launcher %s missing, falling back to `gradle --no-daemon run` in %s",
			AudiverisLauncher, AudiverisHome)
	}
	log.Printf("boot: timeout=%s maxConcurrent=%d maxPages=%d jvmOpts=%q",
		JobTimeout, MaxConcurrentJobs, MaxPDFPages, AudiverisJVMOpts)

	go startCleanupRoutine()

	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/status/", statusHandler)
	http.HandleFunc("/download/", downloadHandler)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("/app/templates"))))

	log.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func launcherAvailable() bool {
	info, err := os.Stat(AudiverisLauncher)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}

// removeStaleGradleLocks deletes lock files left behind by a Gradle process
// that was killed (or that ran for 34 hours and was killed by hand). This is
// the exact failure that took Audiforge down on 2026-09-18: a hung
// `gradle run` held .gradle/buildOutputCleanup/buildOutputCleanup.lock and
// every later request died with "Timeout waiting to lock Build Output Cleanup
// Cache". Nothing holds those locks at boot, so removing them is always safe.
func removeStaleGradleLocks() {
	roots := []string{filepath.Join(AudiverisHome, ".gradle"), "/root/.gradle"}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if strings.HasSuffix(info.Name(), ".lock") {
				if rmErr := os.Remove(path); rmErr != nil {
					log.Printf("boot: could not remove stale lock %s: %v", path, rmErr)
				} else {
					log.Printf("boot: removed stale Gradle lock %s", path)
				}
			}
			return nil
		})
	}
}

// bootSweep clears anything older than BootSweepTTL from the upload and
// download directories.
func bootSweep() {
	for _, dir := range []string{UploadDir, DownloadDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			if time.Since(info.ModTime()) <= BootSweepTTL {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			if err := os.RemoveAll(path); err != nil {
				log.Printf("boot: sweep failed for %s: %v", path, err)
			} else {
				log.Printf("boot: swept stale entry %s", path)
			}
		}
	}
}

func startCleanupRoutine() {
	for {
		time.Sleep(CleanupInterval)
		cleanupFiles()
	}
}

// cleanupFiles removes finished work older than FileTTL. It used to be a
// no-op unless LOG=debug, which meant the production container never cleaned
// anything up at all.
func cleanupFiles() {
	entries, err := os.ReadDir(UploadDir)
	if err == nil {
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil || time.Since(info.ModTime()) <= FileTTL {
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".pdf") {
				continue
			}
			path := filepath.Join(UploadDir, entry.Name())
			if err := os.Remove(path); err == nil {
				log.Printf("cleanup: removed upload %s", path)
			}
		}
	}

	entries, err = os.ReadDir(DownloadDir)
	if err == nil {
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil || time.Since(info.ModTime()) <= FileTTL {
				continue
			}
			path := filepath.Join(DownloadDir, entry.Name())
			if err := os.RemoveAll(path); err == nil {
				log.Printf("cleanup: removed output %s", path)
				processing.Delete(entry.Name())
			}
		}
	}
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if templates == nil {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "Audiforge is running. POST /upload, GET /status/{id}, GET /download/{id}, GET /health")
		return
	}
	if err := templates.ExecuteTemplate(w, "index.html", nil); err != nil {
		log.Printf("index: %v", err)
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(healthReport{
		Status:          "ok",
		Busy:            busyCount.Load(),
		Queued:          queuedCount.Load(),
		LastSuccessUnix: lastSuccessUnix.Load(),
		LastError:       readLastError(),
	})
}

func writeJSONError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "error",
		"message": message,
	})
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Invalid file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if ext := strings.ToLower(filepath.Ext(header.Filename)); ext != ".pdf" {
		http.Error(w, "Only PDF files allowed", http.StatusBadRequest)
		return
	}

	id := uuid.New().String()
	uploadPath := filepath.Join(UploadDir, id+".pdf")

	out, err := os.Create(uploadPath)
	if err != nil {
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(uploadPath)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}
	if err := out.Close(); err != nil {
		os.Remove(uploadPath)
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	// Page cap. countPDFPages fails open (returns 0) on anything it cannot
	// read confidently, so a compressed-xref PDF is never refused by mistake.
	if MaxPDFPages > 0 {
		if pages := countPDFPages(uploadPath); pages > MaxPDFPages {
			os.Remove(uploadPath)
			msg := fmt.Sprintf("PDF has %d pages, which is over the %d page limit for this service", pages, MaxPDFPages)
			log.Printf("upload: refused %s (%s)", header.Filename, msg)
			recordError(msg)
			writeJSONError(w, http.StatusRequestEntityTooLarge, msg)
			return
		}
	}

	processing.Store(id, ProcessingStatus{
		Status:    "queued",
		Message:   "File uploaded, waiting for a conversion slot",
		Timestamp: time.Now().Unix(),
	})

	go processFile(id, uploadPath)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/status/")
	if !idPattern.MatchString(id) {
		http.Error(w, "Invalid ID", http.StatusNotFound)
		return
	}
	status, ok := processing.Load(id)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/download/")
	if !idPattern.MatchString(id) {
		http.Error(w, "No movements found", http.StatusNotFound)
		return
	}
	outputDir := filepath.Join(DownloadDir, id)

	files, err := filepath.Glob(filepath.Join(outputDir, "*.mxl"))
	if err != nil || len(files) == 0 {
		http.Error(w, "No movements found", http.StatusNotFound)
		return
	}

	zipPath := filepath.Join(outputDir, "converted.zip")
	args := append([]string{"-j", zipPath}, files...)
	cmd := exec.Command("zip", args...)
	if err := cmd.Run(); err != nil {
		http.Error(w, "Failed to create ZIP archive: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", id+".zip"))
	http.ServeFile(w, r, zipPath)
}

// ---------------------------------------------------------------------------
// PDF page counting
// ---------------------------------------------------------------------------

// countPDFPages does a cheap byte scan for page objects. It deliberately does
// not pull in a PDF library: this is a guard rail, not a parser. It returns 0
// when it cannot tell (encrypted, object streams, unreadable), so the caller
// fails open and lets the job through.
func countPDFPages(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("pagecount: cannot read %s: %v", path, err)
		return 0
	}
	n := len(pageObjPattern.FindAll(data, -1))
	log.Printf("pagecount: %s -> %d page objects", filepath.Base(path), n)
	return n
}

// ---------------------------------------------------------------------------
// Conversion
// ---------------------------------------------------------------------------

func processFile(id, inputPath string) {
	defer os.Remove(inputPath)

	outputDir := filepath.Join(DownloadDir, id)
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		failJob(id, fmt.Sprintf("Failed to create output directory: %v", err))
		return
	}

	// Queue for a slot. Status stays "queued" (a processing state for the
	// LalaScript client, which only stops on done/complete/completed/finished
	// or error/failed) until a slot frees up.
	queuedCount.Add(1)
	jobSlots <- struct{}{}
	queuedCount.Add(-1)
	busyCount.Add(1)
	defer func() {
		busyCount.Add(-1)
		<-jobSlots
	}()

	logPath := filepath.Join(outputDir, "conversion.log")
	outputFile, err := os.Create(logPath)
	if err != nil {
		failJob(id, fmt.Sprintf("Failed to create log file: %v", err))
		return
	}
	defer outputFile.Close()

	var logWriter io.Writer = outputFile
	if os.Getenv("LOG") == "debug" {
		logWriter = io.MultiWriter(outputFile, os.Stdout)
	}

	processing.Store(id, ProcessingStatus{
		Status:    "processing",
		Message:   "Converting PDF to MusicXML",
		Timestamp: time.Now().Unix(),
	})

	log.Printf("=== START Processing %s ===", id)
	defer log.Printf("=== END Processing %s ===", id)

	ctx, cancel := context.WithTimeout(context.Background(), JobTimeout)
	defer cancel()

	cmd, lane := buildConversionCommand(ctx, inputPath, outputDir)
	log.Printf("job %s: engine lane = %s (%s)", id, lane, cmd.Path)

	cmd.Stdout = logWriter
	cmd.Stderr = logWriter

	// Put the child in its own process group so that a timeout can take down
	// the whole tree (launcher shell -> java, or gradle -> daemon -> java).
	// Killing only the direct child is what let a 286 page job survive its
	// own client by 34 hours.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		log.Printf("job %s: deadline hit, SIGKILL to process group %d", id, cmd.Process.Pid)
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// Fall back to the single process if the group is already gone.
			return cmd.Process.Kill()
		}
		return nil
	}
	// If the group is killed but a grandchild still holds the log pipe open,
	// stop waiting rather than leaking this goroutine forever.
	cmd.WaitDelay = killGrace

	runErr := cmd.Run()
	timedOut := ctx.Err() == context.DeadlineExceeded

	files, _ := filepath.Glob(filepath.Join(outputDir, "*.mxl"))
	movementCount := len(files)

	if timedOut && movementCount == 0 {
		msg := fmt.Sprintf("Conversion killed after %d s (page count too high?)", int(JobTimeout.Seconds()))
		failJob(id, msg)
		return
	}

	if movementCount > 0 {
		msg := "Conversion completed with potential warnings"
		if timedOut {
			msg = fmt.Sprintf("Conversion killed after %d s but %d movement(s) were exported",
				int(JobTimeout.Seconds()), movementCount)
		} else if runErr != nil {
			msg = fmt.Sprintf("Conversion completed with errors (%v)", runErr)
		}
		lastSuccessUnix.Store(time.Now().Unix())
		processing.Store(id, ProcessingStatus{
			Status:        "completed",
			Message:       msg,
			Timestamp:     time.Now().Unix(),
			MovementCount: movementCount,
		})
		return
	}

	errorMsg := "Conversion failed - no movements generated"
	if runErr != nil {
		errorMsg += fmt.Sprintf(" (exec error: %v)", runErr)
	}
	failJob(id, errorMsg)
}

func failJob(id, message string) {
	log.Printf("job %s failed: %s", id, message)
	recordError(message)
	processing.Store(id, ProcessingStatus{
		Status:    "error",
		Message:   message,
		Timestamp: time.Now().Unix(),
	})
}

// buildConversionCommand prefers the installDist launcher baked into the
// image. Gradle is only used if that launcher is missing, and then only with
// --no-daemon: a Gradle daemon must never be started at request time.
func buildConversionCommand(ctx context.Context, inputPath, outputDir string) (*exec.Cmd, string) {
	cmdArgs := []string{"-batch", "-export", "-output", outputDir, "--", inputPath}

	if launcherAvailable() {
		cmd := exec.CommandContext(ctx, AudiverisLauncher, cmdArgs...)
		cmd.Dir = outputDir
		// The Gradle-generated start script concatenates DEFAULT_JVM_OPTS,
		// JAVA_OPTS and AUDIVERIS_OPTS onto the java command line.
		cmd.Env = append(os.Environ(),
			"JAVA_OPTS="+AudiverisJVMOpts,
			"GRADLE_OPTS=-Dorg.gradle.daemon=false",
		)
		return cmd, "installDist"
	}

	cmd := exec.CommandContext(ctx, "gradle",
		"--no-daemon",
		"run",
		"-PjvmLineArgs="+AudiverisJVMOpts,
		fmt.Sprintf("-PcmdLineArgs=%s", escapeArgs(cmdArgs)),
	)
	cmd.Dir = AudiverisHome
	cmd.Env = append(os.Environ(), "GRADLE_OPTS=-Dorg.gradle.daemon=false")
	return cmd, "gradle-fallback"
}

func escapeArgs(args []string) string {
	var escaped []string
	for _, arg := range args {
		if strings.ContainsAny(arg, " ,") {
			escaped = append(escaped, fmt.Sprintf(`"%s"`, arg))
		} else {
			escaped = append(escaped, arg)
		}
	}
	return strings.Join(escaped, ",")
}
