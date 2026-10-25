package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	mp3BytesPerSec = 24000
	maxQueueTracks = 1000
)

type Config struct {
	Address       string `json:"address"`
	MusicRoot     string `json:"musicRoot"`
	CacheDir      string `json:"cacheDir"`
	UploadDir     string `json:"uploadDir"`
	PublicBase    string `json:"publicBase"`
	StationMode   string `json:"stationMode"`
	StationSerial string `json:"stationSerial"`
	StationHost   string `json:"stationHost"`
	StationPort   int    `json:"stationPort"`
	StationUser   string `json:"stationUser"`
	ConfigPath    string `json:"-"`
}

type Track struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
	Live     bool   `json:"live,omitempty"`

	source string
}

type Queue struct {
	ID        string    `json:"id"`
	Tracks    []Track   `json:"tracks"`
	CreatedAt time.Time `json:"createdAt"`
}

type Active struct {
	QueueID    string
	TrackIndex int
	StartedAt  time.Time
	Cancel     context.CancelFunc
	Cmd        *exec.Cmd
	Done       chan struct{}
	Err        error
	Health     PlaybackHealth
	Stopped    bool
}

type App struct {
	cfgMu sync.RWMutex
	cfg   Config

	queueMu sync.Mutex
	queue   *Queue
	active  *Active
	playMu  sync.Mutex

	cache    *Cache
	token    string
	ffprobe  string
	ffmpeg   string
	metadata metadataCache

	volumeMu   sync.Mutex
	lastVolume *VolumeStatus
}

func main() {
	var (
		flagConfig        = flag.String("config", "station-music-web.json", "path to JSON config")
		flagAddress       = flag.String("address", "", "HTTP listen address")
		flagMusicRoot     = flag.String("music-root", "", "music root directory")
		flagCacheDir      = flag.String("cache-dir", "", "transcoded MP3 cache directory")
		flagUploadDir     = flag.String("upload-dir", "", "uploaded files directory")
		flagPublicBase    = flag.String("public-base", "", "external base URL, e.g. http://192.168.1.10:8080")
		flagStationMode   = flag.String("station-mode", "", "adb or ssh")
		flagStationSerial = flag.String("station-serial", "", "ADB serial, usually IP:5555")
		flagStationHost   = flag.String("station-host", "", "SSH host")
		flagStationPort   = flag.Int("station-port", 0, "SSH port")
		flagStationUser   = flag.String("station-user", "", "SSH user")
	)
	flag.Parse()

	cfg, err := loadConfig(*flagConfig)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	explicit := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	applyFlag := func(name string, set func()) {
		if explicit[name] {
			set()
		}
	}
	applyFlag("address", func() { cfg.Address = *flagAddress })
	applyFlag("music-root", func() { cfg.MusicRoot = *flagMusicRoot })
	applyFlag("cache-dir", func() { cfg.CacheDir = *flagCacheDir })
	applyFlag("upload-dir", func() { cfg.UploadDir = *flagUploadDir })
	applyFlag("public-base", func() { cfg.PublicBase = *flagPublicBase })
	applyFlag("station-mode", func() { cfg.StationMode = *flagStationMode })
	applyFlag("station-serial", func() { cfg.StationSerial = *flagStationSerial })
	applyFlag("station-host", func() { cfg.StationHost = *flagStationHost })
	applyFlag("station-port", func() { cfg.StationPort = *flagStationPort })
	applyFlag("station-user", func() { cfg.StationUser = *flagStationUser })

	envStr := func(dst *string, names ...string) {
		for _, name := range names {
			if v := strings.TrimSpace(os.Getenv(name)); v != "" {
				*dst = v
				return
			}
		}
	}
	envStr(&cfg.Address, "MUSIC_WEB_ADDRESS", "ADDRESS")
	envStr(&cfg.MusicRoot, "MUSIC_ROOT")
	envStr(&cfg.CacheDir, "MUSIC_CACHE_DIR")
	envStr(&cfg.UploadDir, "MUSIC_UPLOAD_DIR")
	envStr(&cfg.PublicBase, "MUSIC_PUBLIC", "PUBLIC_BASE")
	envStr(&cfg.StationMode, "STATION_MODE")
	envStr(&cfg.StationSerial, "STATION_SERIAL")
	envStr(&cfg.StationHost, "STATION_HOST")
	envStr(&cfg.StationUser, "STATION_USER")
	if v := strings.TrimSpace(os.Getenv("STATION_PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.StationPort = n
		}
	}

	cfg.ConfigPath = *flagConfig
	if err := normalizeConfig(&cfg); err != nil {
		log.Fatalf("config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		log.Fatalf("token: %v", err)
	}

	app := &App{
		cfg:     cfg,
		cache:   NewCache(ctx, cfg.CacheDir),
		token:   hex.EncodeToString(tokenBytes),
		ffprobe: findTool("ffprobe"),
		ffmpeg:  findTool("ffmpeg"),
	}

	mux := http.NewServeMux()
	mux.Handle("/", app.staticHandler())
	mux.HandleFunc("GET /api/config", app.getConfig)
	mux.HandleFunc("POST /api/config", app.updateConfig)
	mux.HandleFunc("GET /api/library", app.library)
	mux.HandleFunc("POST /api/library/open", app.openPath)
	mux.HandleFunc("POST /api/upload", app.upload)
	mux.HandleFunc("GET /api/queue", app.getQueue)
	mux.HandleFunc("POST /api/queue", app.updateQueue)
	mux.HandleFunc("DELETE /api/queue", app.clearQueue)
	mux.HandleFunc("GET /api/status", app.getStatus)
	mux.HandleFunc("POST /api/control", app.control)
	mux.HandleFunc("GET /api/volume", app.getVolume)
	mux.HandleFunc("POST /api/volume", app.setVolume)
	mux.HandleFunc("GET /music/q/{queue}/{track}", app.serveQueue)

	srv := &http.Server{
		Addr:              cfg.Address,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Station Music Web: http://%s (music root: %s)", displayListen(cfg.Address), cfg.MusicRoot)
		serverErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http: %v", err)
		}
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	app.stopPlayback(false)
	app.cache.Close()
}

func loadConfig(path string) (Config, error) {
	cwd, err := filepath.Abs(".")
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Address:     ":8080",
		MusicRoot:   cwd,
		CacheDir:    filepath.Join(cwd, ".station-music-cache"),
		UploadDir:   filepath.Join(cwd, "uploads"),
		StationMode: "adb",
		StationPort: 2222,
		StationUser: "root",
		ConfigPath:  path,
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	cfg.ConfigPath = path
	return cfg, nil
}

func normalizeConfig(cfg *Config) error {
	if cfg.Address == "" {
		cfg.Address = ":8080"
	}
	if _, _, err := net.SplitHostPort(cfg.Address); err != nil {
		return fmt.Errorf("invalid address %q: %w", cfg.Address, err)
	}
	if cfg.MusicRoot == "" {
		return errors.New("music root is empty")
	}
	if !filepath.IsAbs(cfg.MusicRoot) {
		abs, err := filepath.Abs(cfg.MusicRoot)
		if err != nil {
			return err
		}
		cfg.MusicRoot = abs
	}
	if !filepath.IsAbs(cfg.UploadDir) {
		cfg.UploadDir = filepath.Join(cfg.MusicRoot, cfg.UploadDir)
	}
	// Uploaded files must be visible in the music library. Keep the default
	// upload directory inside the active root even when the root is changed
	// from the UI; otherwise newly uploaded tracks cannot be opened again.
	if !isPathWithin(cfg.MusicRoot, cfg.UploadDir) {
		cfg.UploadDir = filepath.Join(cfg.MusicRoot, "uploads")
	}
	if !filepath.IsAbs(cfg.CacheDir) {
		abs, err := filepath.Abs(cfg.CacheDir)
		if err != nil {
			return err
		}
		cfg.CacheDir = abs
	}
	for _, dir := range []string{cfg.MusicRoot, cfg.CacheDir, cfg.UploadDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	cfg.StationMode = strings.ToLower(strings.TrimSpace(cfg.StationMode))
	if cfg.StationMode == "" {
		cfg.StationMode = "adb"
	}
	if cfg.StationMode != "adb" && cfg.StationMode != "ssh" {
		return fmt.Errorf("station mode must be adb or ssh, got %q", cfg.StationMode)
	}
	if cfg.StationPort <= 0 {
		cfg.StationPort = 2222
	}
	if cfg.StationUser == "" {
		cfg.StationUser = "root"
	}
	if cfg.PublicBase != "" && !strings.HasPrefix(cfg.PublicBase, "http://") && !strings.HasPrefix(cfg.PublicBase, "https://") {
		cfg.PublicBase = "http://" + cfg.PublicBase
	}
	return nil
}

func isPathWithin(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if filepath.IsAbs(root) != filepath.IsAbs(path) {
		return false
	}
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

func saveConfig(cfg Config) error {
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := cfg.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, cfg.ConfigPath); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (a *App) config() Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func displayListen(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		return net.JoinHostPort("127.0.0.1", port)
	default:
		return addr
	}
}

func findTool(name string) string {
	if p := strings.TrimSpace(os.Getenv(strings.ToUpper(name) + "_PATH")); p != "" {
		return p
	}
	return name
}

func randomHex(bytesLen int) string {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

//go:embed static
var staticFiles embed.FS

func (a *App) staticHandler() http.Handler {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-store")
		}
		files.ServeHTTP(w, r)
	})
}
