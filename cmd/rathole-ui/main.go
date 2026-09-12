package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
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

//go:embed web/*
var embeddedWebFS embed.FS

// Maximum allowed payload sizes
const (
	MaxConfigPayloadBytes = 2 * 1024 * 1024 // 2MB
	MaxActionPayloadBytes = 64 * 1024        // 64KB
	MaxLogFileSize        = 3 * 1024 * 1024 // 3MB (auto-truncate threshold)
)

// Manager coordinates the lifecycle of the rathole child process,
// configuration file persistence, and process log streams.
type Manager struct {
	mu         sync.Mutex
	binPath    string
	configPath string
	logPath    string
	pidPath    string

	cmd       *exec.Cmd
	startTime time.Time
	lastErr   string
}

// NewManager creates an instance of Manager with cleaned file paths.
func NewManager(binPath, configPath, logPath, pidPath string) *Manager {
	return &Manager{
		binPath:    filepath.Clean(binPath),
		configPath: filepath.Clean(configPath),
		logPath:    filepath.Clean(logPath),
		pidPath:    filepath.Clean(pidPath),
	}
}

// isRunningLocked checks process status via signal 0 (caller must hold mu).
func (m *Manager) isRunningLocked() bool {
	if m.cmd != nil && m.cmd.Process != nil {
		if err := m.cmd.Process.Signal(syscall.Signal(0)); err == nil {
			return true
		}
	}
	// Fallback check via PID file if process was externally spawned or survived
	if m.pidPath != "" {
		if data, err := os.ReadFile(m.pidPath); err == nil {
			pidStr := strings.TrimSpace(string(data))
			if pid, err := strconv.Atoi(pidStr); err == nil && pid > 0 {
				if proc, err := os.FindProcess(pid); err == nil {
					if err := proc.Signal(syscall.Signal(0)); err == nil {
						return true
					}
				}
			}
		}
	}
	return false
}

// IsRunning reports whether the rathole daemon process is currently running.
func (m *Manager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isRunningLocked()
}

// Start launches the rathole binary as a background child process.
func (m *Manager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.isRunningLocked() {
		return fmt.Errorf("rathole 已在运行中")
	}

	if _, err := os.Stat(m.binPath); os.IsNotExist(err) {
		m.lastErr = fmt.Sprintf("二进制文件不存在: %s", m.binPath)
		return fmt.Errorf("%s", m.lastErr)
	}

	if _, err := os.Stat(m.configPath); os.IsNotExist(err) {
		m.lastErr = fmt.Sprintf("配置文件不存在: %s", m.configPath)
		return fmt.Errorf("%s", m.lastErr)
	}

	// Ensure parent directory for log exists with 0775 permissions
	_ = os.MkdirAll(filepath.Dir(m.logPath), 0775)
	_ = os.Chmod(m.logPath, 0664)

	// Truncate log if it exceeds 3MB to prevent disk exhaustion
	if fi, err := os.Stat(m.logPath); err == nil && fi.Size() > MaxLogFileSize {
		m.truncateLogLocked(1500)
	}

	logFile, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0664)
	if err != nil {
		log.Printf("[Rathole-UI] Warning: cannot open %s (%v), attempting fallback to /tmp/rathole.log", m.logPath, err)
		m.logPath = "/tmp/rathole.log"
		_ = os.Chmod(m.logPath, 0664)
		logFile, err = os.OpenFile(m.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0664)
		if err != nil {
			log.Printf("[Rathole-UI] Warning: cannot open fallback log %s: %v. Using stdout.", m.logPath, err)
			logFile = nil
		}
	}

	cmd := exec.Command(m.binPath, m.configPath)
	if logFile != nil {
		cmd.Stdout = logFile
		cmd.Stderr = logFile
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	cmd.Env = append(os.Environ(), "RUST_LOG=rathole=info,info")

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		m.lastErr = fmt.Sprintf("启动 rathole 核心失败: %v", err)
		return err
	}

	m.cmd = cmd
	m.startTime = time.Now()
	m.lastErr = ""

	if m.pidPath != "" {
		_ = os.WriteFile(m.pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0664)
	}

	// Asynchronously monitor child process termination
	go func(c *exec.Cmd, f *os.File) {
		_ = c.Wait()
		if f != nil {
			_ = f.Close()
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.cmd == c {
			m.cmd = nil
			if m.pidPath != "" {
				_ = os.Remove(m.pidPath)
			}
		}
	}(cmd, logFile)

	return nil
}

// Stop gracefully signals the rathole process with SIGTERM, falling back to Kill after 3s.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	var p *os.Process
	if m.cmd != nil && m.cmd.Process != nil {
		p = m.cmd.Process
	} else if m.pidPath != "" {
		data, err := os.ReadFile(m.pidPath)
		if err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				p, _ = os.FindProcess(pid)
			}
		}
	}

	if p == nil {
		return nil
	}

	_ = p.Signal(syscall.SIGTERM)

	// Wait up to 3 seconds for graceful exit
	done := make(chan struct{})
	go func() {
		for i := 0; i < 30; i++ {
			if err := p.Signal(syscall.Signal(0)); err != nil {
				close(done)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = p.Kill()
		close(done)
	}()

	<-done
	m.cmd = nil
	if m.pidPath != "" {
		_ = os.Remove(m.pidPath)
	}
	return nil
}

// Restart stops and starts the daemon.
func (m *Manager) Restart() error {
	_ = m.Stop()
	time.Sleep(300 * time.Millisecond)
	return m.Start()
}

// truncateLogLocked keeps the last keepLines lines of the log file (caller must hold mu).
func (m *Manager) truncateLogLocked(keepLines int) {
	data, err := os.ReadFile(m.logPath)
	if err != nil {
		return
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > keepLines {
		truncated := strings.Join(lines[len(lines)-keepLines:], "\n")
		_ = os.WriteFile(m.logPath, []byte(truncated), 0664)
	}
}

// GetStatus returns the current runtime metadata for status reporting.
func (m *Manager) GetStatus() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	running := m.isRunningLocked()
	pid := 0
	uptimeStr := "-"

	if running {
		if m.cmd != nil && m.cmd.Process != nil {
			pid = m.cmd.Process.Pid
		} else if m.pidPath != "" {
			if data, err := os.ReadFile(m.pidPath); err == nil {
				pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			}
		}
		if !m.startTime.IsZero() {
			dur := time.Since(m.startTime).Round(time.Second)
			h := int(dur.Hours())
			min := int(dur.Minutes()) % 60
			sec := int(dur.Seconds()) % 60
			if h > 0 {
				uptimeStr = fmt.Sprintf("%dh %dm %ds", h, min, sec)
			} else if min > 0 {
				uptimeStr = fmt.Sprintf("%dm %ds", min, sec)
			} else {
				uptimeStr = fmt.Sprintf("%ds", sec)
			}
		} else {
			uptimeStr = "运行中"
		}
	}

	var logSize int64 = 0
	if fi, err := os.Stat(m.logPath); err == nil {
		logSize = fi.Size()
	}

	return map[string]interface{}{
		"running":     running,
		"pid":         pid,
		"uptime":      uptimeStr,
		"last_error":  m.lastErr,
		"config_path": m.configPath,
		"log_path":    m.logPath,
		"log_size":    logSize,
	}
}

// ReadLog reads up to maxLines from the current log file.
func (m *Manager) ReadLog(maxLines int) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.logPath)
	if err != nil && m.logPath != "/tmp/rathole.log" {
		if tmpData, tmpErr := os.ReadFile("/tmp/rathole.log"); tmpErr == nil && len(tmpData) > 0 {
			data = tmpData
			err = nil
		}
	}
	if err != nil {
		return fmt.Sprintf("[Rathole-UI] 日志文件暂无内容或未生成 (%s)", m.logPath)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > maxLines {
		return strings.Join(lines[len(lines)-maxLines:], "\n")
	}
	return string(data)
}

// ClearLog empties the log files.
func (m *Manager) ClearLog() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	_ = os.Chmod(m.logPath, 0664)
	_ = os.WriteFile("/tmp/rathole.log", []byte(""), 0664)
	_ = os.WriteFile("/tmp/rathole-ui.log", []byte(""), 0664)

	err := os.WriteFile(m.logPath, []byte(""), 0664)
	if err != nil && m.logPath != "/tmp/rathole.log" {
		log.Printf("[Rathole-UI] Warning: cannot truncate %s (%v), using /tmp/rathole.log", m.logPath, err)
		m.logPath = "/tmp/rathole.log"
		err = os.WriteFile(m.logPath, []byte(""), 0664)
	}
	return err
}

// ReadConfig reads the configuration file content from disk.
func (m *Manager) ReadConfig() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := os.ReadFile(m.configPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// SaveConfig safely writes the configuration file.
func (m *Manager) SaveConfig(content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := filepath.Dir(m.configPath)
	if err := os.MkdirAll(dir, 0775); err != nil {
		log.Printf("[Rathole-UI] Warning: MkdirAll %s: %v", dir, err)
	}

	// Strategy 1: Direct file write (works when file exists with 0664)
	var writeErr error
	f, err := os.OpenFile(m.configPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0664)
	if err == nil {
		_, writeErr = f.WriteString(content)
		_ = f.Sync()
		_ = f.Close()
		if writeErr == nil {
			log.Printf("[Rathole-UI] Config successfully saved to %s (%d bytes)", m.configPath, len(content))
			return nil
		}
	}

	// Strategy 2: Atomic write via temp file + rename
	tmpPath := m.configPath + ".tmp"
	if errTmp := os.WriteFile(tmpPath, []byte(content), 0664); errTmp == nil {
		if errRename := os.Rename(tmpPath, m.configPath); errRename == nil {
			log.Printf("[Rathole-UI] Config successfully saved via rename to %s (%d bytes)", m.configPath, len(content))
			return nil
		}
		_ = os.Remove(tmpPath)
	}

	if err != nil {
		return fmt.Errorf("failed writing config to %s: %w", m.configPath, err)
	}
	return fmt.Errorf("failed writing config to %s: %v", m.configPath, writeErr)
}

// =============================================================================
// HTTP Handlers & Router
// =============================================================================

// writeJSON formats and writes a JSON response with status code.
func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// securityMiddleware injects baseline security headers on every response.
func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

// setupRouter configures HTTP routes and handler bindings.
func setupRouter(mgr *Manager) (http.Handler, error) {
	mux := http.NewServeMux()

	// Static UI assets served from embedded filesystem
	subFS, err := fs.Sub(embeddedWebFS, "web")
	if err != nil {
		return nil, fmt.Errorf("failed to load embedded web assets: %w", err)
	}
	mux.Handle("/", http.FileServer(http.FS(subFS)))

	// GET /api/status - Query daemon and process status
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
				"success": false,
				"message": "Method not allowed",
			})
			return
		}
		writeJSON(w, http.StatusOK, mgr.GetStatus())
	})

	// GET & POST /api/config - Read or save configuration
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			content, err := mgr.ReadConfig()
			if err != nil {
				content = ""
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"success": true,
				"content": content,
			})

		case http.MethodPost:
			// Limit body to 2MB to prevent DoS
			r.Body = http.MaxBytesReader(w, r.Body, MaxConfigPayloadBytes)

			var body struct {
				Content string `json:"content"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"message": "无效的 JSON 请求体或超出大小限制 (最大 2MB)",
				})
				return
			}
			if err := mgr.SaveConfig(body.Content); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
					"success": false,
					"message": fmt.Sprintf("保存配置失败: %v", err),
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"success": true,
				"message": "配置文件已成功保存",
			})

		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
				"success": false,
				"message": "Method not allowed",
			})
		}
	})

	// POST /api/action - Process control (start, stop, restart)
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
				"success": false,
				"message": "Method not allowed",
			})
			return
		}

		// Limit body to 64KB
		r.Body = http.MaxBytesReader(w, r.Body, MaxActionPayloadBytes)

		var body struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": "无效的指令请求",
			})
			return
		}

		var actionErr error
		msg := ""
		switch body.Action {
		case "start":
			actionErr = mgr.Start()
			msg = "服务已启动"
		case "stop":
			actionErr = mgr.Stop()
			msg = "服务已停止"
		case "restart":
			actionErr = mgr.Restart()
			msg = "服务已重启"
		default:
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("未知操作指令: %s", body.Action),
			})
			return
		}

		if actionErr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": actionErr.Error(),
			})
			return
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": msg,
		})
	})

	// GET /api/log - Fetch recent logs with line limits
	mux.HandleFunc("/api/log", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
				"success": false,
				"message": "Method not allowed",
			})
			return
		}

		lines := 400
		if q := r.URL.Query().Get("lines"); q != "" {
			if n, err := strconv.Atoi(q); err == nil && n > 0 && n <= 2000 {
				lines = n
			}
		}
		content := mgr.ReadLog(lines)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"log":     content,
		})
	})

	// POST /api/clear_log - Clear log files
	mux.HandleFunc("/api/clear_log", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
				"success": false,
				"message": "Method not allowed",
			})
			return
		}

		if err := mgr.ClearLog(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("清空日志失败: %v", err),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": "日志已清空",
		})
	})

	return securityMiddleware(mux), nil
}

func main() {
	listenAddr := flag.String("listen", ":8520", "HTTP WebUI listen address")
	binPath := flag.String("bin", "/var/packages/rathole/target/bin/rathole", "Path to rathole binary")
	configPath := flag.String("config", "/var/packages/rathole/var/config.toml", "Path to config.toml")
	logPath := flag.String("log", "/var/packages/rathole/var/rathole.log", "Path to rathole log file")
	pidPath := flag.String("pid", "/var/packages/rathole/var/rathole.pid", "Path to rathole pid file")
	flag.Parse()

	// Ensure runtime directories exist with 0775
	_ = os.MkdirAll(filepath.Dir(*configPath), 0775)
	_ = os.MkdirAll(filepath.Dir(*logPath), 0775)
	_ = os.Chmod(*configPath, 0664)
	_ = os.Chmod(*logPath, 0664)

	mgr := NewManager(*binPath, *configPath, *logPath, *pidPath)

	// Attempt auto-starting rathole if configuration is present
	if _, err := os.Stat(*configPath); err == nil {
		_ = mgr.Start()
	}

	handler, err := setupRouter(mgr)
	if err != nil {
		log.Fatalf("[Rathole-UI] Router initialization failed: %v", err)
	}

	// Configure hardened HTTP server with explicit timeouts to prevent Slowloris / DoS
	srv := &http.Server{
		Addr:              *listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1MB
	}

	// Graceful shutdown handling for SIGINT & SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("[Rathole-UI] Received signal %v, commencing graceful shutdown...", sig)

		// 1. Terminate incoming HTTP traffic with 5s timeout
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("[Rathole-UI] HTTP server shutdown error: %v", err)
		}

		// 2. Terminate rathole child process cleanly
		log.Println("[Rathole-UI] Stopping rathole child process...")
		_ = mgr.Stop()

		os.Exit(0)
	}()

	log.Printf("[Rathole-UI] Listening on %s ...\n", *listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("[Rathole-UI] Server error: %v", err)
	}
}
