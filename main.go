package main

import (
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
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	UploadDir   = "/tmp/uploads"
	DownloadDir = "/tmp/downloads"
	CleanupInterval = 1 * time.Hour
	FileTTL     = 1 * time.Hour

	// LAL-546: OCR language switch.
	// AUDIVERIS_OCR_LANGS empty  -> no OCR languages at all (exactly prod before LAL-546):
	//                               TESSDATA_PREFIX is removed from the engine's environment.
	// AUDIVERIS_OCR_LANGS "a+b"  -> TESSDATA_PREFIX is kept and the engine gets
	//                               -constant org.audiveris.omr.text.Language.defaultSpecification=a+b
	OcrLangsEnv         = "AUDIVERIS_OCR_LANGS"
	TessdataPrefixEnv   = "TESSDATA_PREFIX"
	OcrLanguageConstant = "org.audiveris.omr.text.Language.defaultSpecification"
)

// Tesseract codes joined by '+', e.g. eng+deu+fra or chi_sim. No commas or spaces: gradle
// splits -PcmdLineArgs on ',' (app/build.gradle: args cmdLineArgs.split(',')).
var ocrLangsPattern = regexp.MustCompile(`^[a-z]{3}(_[a-z]+)?(\+[a-z]{3}(_[a-z]+)?)*$`)

type ProcessingStatus struct {
	Status        string `json:"status"`
	Message       string `json:"message"`
	Timestamp     int64  `json:"timestamp"`
	MovementCount int    `json:"movementCount,omitempty"`
}

var (
	processing sync.Map
	templates  *template.Template
)

func init() {
	os.MkdirAll(UploadDir, 0755)
	os.MkdirAll(DownloadDir, 0755)
	templates = template.Must(template.ParseGlob("/app/templates/*.html"))
	go startCleanupRoutine()
}

// I can't believe this Janky code works. Good luck to whoever reads this.
func main() {
	http.HandleFunc("/", indexHandler)
	http.HandleFunc("/upload", uploadHandler)
	http.HandleFunc("/status/", statusHandler)
	http.HandleFunc("/download/", downloadHandler)
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("/app/templates"))))

	logOcrConfig()
	log.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// Add the cleanup routine
func startCleanupRoutine() {
	for {
		time.Sleep(CleanupInterval)
		cleanupFiles()
	}
}

func cleanupFiles() {
    if os.Getenv("LOG") != "debug" {
        return
    }
    
    log.Println("Starting cleanup routine...")
    cleanupDirectory(UploadDir, cleanUploads)
    cleanupDirectory(DownloadDir, cleanDownloads)
}

func cleanupDirectory(path string, cleanFunc func(string) error) {
    err := filepath.Walk(path, func(filePath string, info os.FileInfo, err error) error {
        if err != nil {
            return nil // Skip unreadable files
        }

        // Check if the file is older than TTL
        if time.Since(info.ModTime()) > FileTTL {
            // Call the cleanup function for old files
            if err := cleanFunc(filePath); err != nil {
                log.Printf("Cleanup failed for %s: %v", filePath, err)
            }
        }
        return nil
    })

    if err != nil {
        log.Printf("Cleanup error: %v", err)
    }
}

func cleanUploads(path string) error {
	if strings.HasSuffix(path, ".pdf") {
		if err := os.Remove(path); err == nil {
			log.Printf("Cleaned up upload file: %s", path)
		}
	}
	return nil
}

func cleanDownloads(path string) error {
    // Resolve absolute paths to handle symlinks/relative paths
    downloadDirAbs, err := filepath.Abs(DownloadDir)
    if err != nil {
        return fmt.Errorf("failed to resolve DownloadDir: %v", err)
    }

    fileDir := filepath.Dir(path)
    fileDirAbs, err := filepath.Abs(fileDir)
    if err != nil {
        return nil // Skip if path resolution fails
    }

    // Check if the directory is a subdirectory of DownloadDir
    if strings.HasPrefix(fileDirAbs, downloadDirAbs + string(filepath.Separator)) &&
        fileDirAbs != downloadDirAbs {

        // Delete the entire subdirectory
        if err := os.RemoveAll(fileDir); err != nil {
            return fmt.Errorf("failed to delete %s: %v", fileDir, err)
        }
        log.Printf("Cleaned up download directory: %s", fileDir)
        return filepath.SkipDir // Skip further processing of this directory
    }

    return nil
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	templates.ExecuteTemplate(w, "index.html", nil)
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Invalid file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	if ext := filepath.Ext(header.Filename); ext != ".pdf" {
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
	defer out.Close()

	if _, err := io.Copy(out, file); err != nil {
		http.Error(w, "Failed to save file", http.StatusInternalServerError)
		return
	}

	processing.Store(id, ProcessingStatus{
		Status:    "processing",
		Message:   "File uploaded, starting conversion",
		Timestamp: time.Now().Unix(),
	})

	go processFile(id, uploadPath)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func processFile(id, inputPath string) {
    outputDir := filepath.Join(DownloadDir, id)
    if err := os.MkdirAll(outputDir, 0755); err != nil {
        log.Printf("Error creating output dir: %v", err)
        processing.Store(id, ProcessingStatus{
            Status:    "error",
            Message:   fmt.Sprintf("Failed to create output directory: %v", err),
            Timestamp: time.Now().Unix(),
        })
        return
    }

	ocrLangs := ocrLanguages()

	cmdArgs := []string{
		"-batch",
		"-export",
		"-output", outputDir,
	}
	if ocrLangs != "" {
		cmdArgs = append(cmdArgs, "-constant", OcrLanguageConstant+"="+ocrLangs)
	}
	cmdArgs = append(cmdArgs, "--", inputPath)

	cmd := exec.Command(
		"gradle",
		"run",
		"-PjvmLineArgs=-Xmx3g",
		fmt.Sprintf("-PcmdLineArgs=%s", escapeArgs(cmdArgs)),
	)
	cmd.Dir = "/app/audiveris"
	cmd.Env = engineEnv(ocrLangs)
	log.Printf("Job %s OCR languages: %q", id, ocrLangs)

    logPath := filepath.Join(outputDir, "conversion.log")
    outputFile, err := os.Create(logPath)
    if err != nil {
        log.Printf("Error creating log file: %v", err)
        processing.Store(id, ProcessingStatus{
            Status:    "error",
            Message:   fmt.Sprintf("Failed to create log file: %v", err),
            Timestamp: time.Now().Unix(),
        })
        return
    }
    defer outputFile.Close()

	var logWriter io.Writer = outputFile
    if os.Getenv("LOG") == "debug" {
        logWriter = io.MultiWriter(outputFile, os.Stdout)
    }

    cmd.Stdout = logWriter
    cmd.Stderr = logWriter

	log.Printf("\n=== START Processing %s ===", id)
	defer log.Printf("=== END Processing %s ===\n", id)

	processing.Store(id, ProcessingStatus{
		Status:    "processing",
		Message:   "Converting PDF to MusicXML",
		Timestamp: time.Now().Unix(),
	})

	runErr := cmd.Run()
	
	// Check for generated movements
	files, _ := filepath.Glob(filepath.Join(outputDir, "*.mxl"))
	movementCount := len(files)
	
	// Update processFile to check for both errors and files
if movementCount > 0 {
    msg := "Conversion completed with potential warnings"
    status := "completed"
    if runErr != nil {
        msg = fmt.Sprintf("Conversion completed with errors (%v)", runErr)
    }
    processing.Store(id, ProcessingStatus{
        Status:        status,
        Message:       msg,
        Timestamp:     time.Now().Unix(),
        MovementCount: movementCount,
    })
} else {
		errorMsg := "Conversion failed - no movements generated"
		if runErr != nil {
			errorMsg += fmt.Sprintf(" (exec error: %v)", runErr)
		}
		processing.Store(id, ProcessingStatus{
			Status:    "error",
			Message:   errorMsg,
			Timestamp: time.Now().Unix(),
		})
	}
}

// ocrLanguages returns the validated AUDIVERIS_OCR_LANGS value, or "" (OCR off).
func ocrLanguages() string {
	langs := strings.TrimSpace(os.Getenv(OcrLangsEnv))
	if langs == "" {
		return ""
	}
	if !ocrLangsPattern.MatchString(langs) {
		log.Printf("WARNING: %s=%q is not a '+'-joined list of Tesseract codes; OCR stays OFF", OcrLangsEnv, langs)
		return ""
	}
	return langs
}

// engineEnv is the environment of the Audiveris child process. With OCR off it drops
// TESSDATA_PREFIX, so Audiveris falls back to its own (empty) config tessdata folder and
// logs "No installed OCR languages", as prod did before LAL-546.
func engineEnv(ocrLangs string) []string {
	if ocrLangs != "" {
		return os.Environ()
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, TessdataPrefixEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// logOcrConfig prints the OCR arm at startup, with the language files actually present.
func logOcrConfig() {
	langs := ocrLanguages()
	if langs == "" {
		log.Printf("OCR: OFF (%s empty); engine runs without %s, no OCR languages", OcrLangsEnv, TessdataPrefixEnv)
		return
	}
	prefix := os.Getenv(TessdataPrefixEnv)
	files, _ := filepath.Glob(filepath.Join(prefix, "*.traineddata"))
	var installed []string
	for _, f := range files {
		installed = append(installed, strings.TrimSuffix(filepath.Base(f), ".traineddata"))
	}
	log.Printf("OCR: ON, %s=%s, %s=%s, installed: %s", OcrLangsEnv, langs, TessdataPrefixEnv, prefix, strings.Join(installed, ","))
	for _, code := range strings.Split(langs, "+") {
		if _, err := os.Stat(filepath.Join(prefix, code+".traineddata")); err != nil {
			// Audiveris refuses the whole spec if one code is missing (TesseractOCR.supports).
			log.Printf("WARNING: %s.traineddata not found in %s; Audiveris will skip OCR entirely", code, prefix)
		}
	}
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

func statusHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/status/")
	status, ok := processing.Load(id)
	if !ok {
		http.Error(w, "Invalid ID", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
    id := strings.TrimPrefix(r.URL.Path, "/download/")
    outputDir := filepath.Join(DownloadDir, id)
    
    // Find all MXL files
    files, err := filepath.Glob(filepath.Join(outputDir, "*.mxl"))
    if err != nil || len(files) == 0 {
        http.Error(w, "No movements found", http.StatusNotFound)
        return
    }
    
    // Create ZIP archive
    zipPath := filepath.Join(outputDir, "converted.zip")
    args := append([]string{"-j", zipPath}, files...)
    cmd := exec.Command("zip", args...)
    if err := cmd.Run(); err != nil {
        http.Error(w, "Failed to create ZIP archive: " + err.Error(), http.StatusInternalServerError)
        return
    }

    // Serve ZIP file
    w.Header().Set("Content-Type", "application/zip")
    w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s.zip\"", id))
    http.ServeFile(w, r, zipPath)
}
