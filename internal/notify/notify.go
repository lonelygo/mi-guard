// Package notify delivers optional macOS notifications through the bundled
// user-session helper without blocking guard decisions.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
)

const (
	helperExecutable = "mi-guard-notify"
	helperApp        = "mi-guard.app"
	queueCapacity    = 16
	deliveryTimeout  = 5 * time.Second
)

type Sender struct {
	config     config.Notifications
	configPath string
	logger     *slog.Logger
	queue      chan message

	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc
}

type message struct {
	kind  string
	title string
	body  string
}

func New(configuration config.Notifications, configPath string, logger *slog.Logger) *Sender {
	if logger == nil {
		logger = slog.Default()
	}
	if configPath != "" {
		// Notification Center delivers the click after the sending process exits.
		configPath, _ = filepath.Abs(configPath)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sender := &Sender{ctx: ctx, cancel: cancel,
		config:     configuration,
		configPath: configPath,
		logger:     logger,
		queue:      make(chan message, queueCapacity),
	}
	if configuration.Enabled {
		sender.wg.Add(1)
		go sender.run()
	}
	return sender
}

// Send queues a notification when its configured class is enabled. A full
// queue drops the message and logs it rather than delaying health checks.
func (s *Sender) Send(kind, title, body string) {
	if !s.allowed(kind) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- message{kind: kind, title: title, body: body}:
	default:
		s.logger.Warn("notification queue full", "kind", kind, "title", title)
	}
}

// Close drains queued notifications and waits for their bounded delivery
// attempts. It is safe to call more than once.
func (s *Sender) Close() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		if s.config.Enabled {
			close(s.queue)
		}
	}
	s.mu.Unlock()
	timer := time.AfterFunc(deliveryTimeout, s.cancel)
	s.wg.Wait()
	timer.Stop()
	s.cancel()
}

// Authorize explicitly asks the helper to request notification permission.
// Automatic Send calls never invoke this path.
func Authorize(ctx context.Context, helper string) error {
	currentExecutable, _ := os.Executable()
	home, _ := os.UserHomeDir()
	executable, err := resolveHelperFrom(helper, currentExecutable, home)
	if err != nil {
		return err
	}
	return runAuthorizationCommand(ctx, executable, os.Stdout)
}

func runAuthorizationCommand(ctx context.Context, executable string, stdout io.Writer) error {
	command := exec.CommandContext(ctx, executable, "--authorize")
	var diagnostics bytes.Buffer
	command.Stdout = stdout
	command.Stderr = &diagnostics
	err := command.Run()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		detail := strings.TrimSpace(diagnostics.String())
		if detail == "" {
			return fmt.Errorf("authorize notifications: %w", err)
		}
		return fmt.Errorf("authorize notifications: %w: %s", err, detail)
	}
	return nil
}

func (s *Sender) allowed(kind string) bool {
	if !s.config.Enabled {
		return false
	}
	switch kind {
	case "summary":
		return s.config.Summary
	case "switch":
		return s.config.Switch
	default:
		return true
	}
}

func (s *Sender) run() {
	defer s.wg.Done()
	for item := range s.queue {
		if s.ctx.Err() != nil {
			return
		}
		if err := s.deliver(item); err != nil {
			s.logger.Warn("notification delivery failed", "kind", item.kind, "error", err)
		}
	}
}

func (s *Sender) deliver(item message) error {
	currentExecutable, _ := os.Executable()
	home, _ := os.UserHomeDir()
	executable, err := resolveHelperFrom(s.config.Helper, currentExecutable, home)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, deliveryTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, executable,
		"--send",
		"--kind", item.kind,
		"--title", item.title,
		"--body", item.body,
	)
	command.Env = append(os.Environ(), "MI_GUARD_NOTIFICATION_CONFIG="+s.configPath)
	output, err := command.CombinedOutput()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("notification helper timed out: %w", ctx.Err())
		}
		return fmt.Errorf("notification helper: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func resolveHelperFrom(helper, executable, home string) (string, error) {
	if helper == "" {
		return "", errors.New("notification helper is not configured")
	}
	if helper != config.AutoNotificationHelper {
		resolved, err := resolveHelperPath(helper)
		if err == nil {
			return resolved, nil
		}
		// Configs generated before layout-aware discovery used this exact path.
		// Preserve it when present, but let an absent legacy default discover a
		// Homebrew installation. Every other configured path remains authoritative.
		legacy := filepath.Join(home, "Applications", helperApp)
		if home == "" || filepath.Clean(helper) != legacy || !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	var candidates []string
	if executable != "" {
		cleanExecutable := filepath.Clean(executable)
		if resolved, err := filepath.EvalSymlinks(cleanExecutable); err == nil {
			cleanExecutable = resolved
		}
		dir := filepath.Dir(cleanExecutable)
		candidates = append(candidates,
			filepath.Clean(filepath.Join(dir, "..", "libexec", helperApp)),
			filepath.Join(dir, helperApp),
		)
	}
	if home != "" {
		homeApp := filepath.Join(home, "Applications", helperApp)
		if len(candidates) == 0 || candidates[len(candidates)-1] != homeApp {
			candidates = append(candidates, homeApp)
		}
	}
	if len(candidates) == 0 {
		return "", errors.New("notification helper auto-discovery has no candidate paths")
	}

	var failures []error
	for _, candidate := range candidates {
		resolved, err := resolveHelperPath(candidate)
		if err == nil {
			return resolved, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", candidate, err))
	}
	return "", fmt.Errorf("notification helper not found in auto-discovery paths: %w", errors.Join(failures...))
}

func resolveHelperPath(helper string) (string, error) {
	clean := filepath.Clean(helper)
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("stat notification helper: %w", err)
	}
	if info.IsDir() || strings.EqualFold(filepath.Ext(clean), ".app") {
		clean = filepath.Join(clean, "Contents", "MacOS", helperExecutable)
		info, err = os.Stat(clean)
		if err != nil {
			return "", fmt.Errorf("stat notification executable: %w", err)
		}
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("notification helper is not executable")
	}
	return clean, nil
}
