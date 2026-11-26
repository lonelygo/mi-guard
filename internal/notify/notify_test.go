package notify

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
)

func TestSenderFiltersAndDrainsEnabledKinds(t *testing.T) {
	app, output := fakeApp(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	sender := New(config.Notifications{
		Enabled: true,
		Summary: false,
		Switch:  true,
		Helper:  app,
	}, "", logger)
	sender.Send("summary", "hidden", "hidden")
	sender.Send("switch", "US changed", "old to new")
	sender.Close()
	sender.Send("switch", "after close", "ignored")

	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got := string(contents)
	for _, expected := range []string{"--send", "--kind", "switch", "--title", "US changed", "--body", "old to new"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("helper output %q does not contain %q", got, expected)
		}
	}
	if strings.Contains(got, "hidden") || strings.Contains(got, "after close") {
		t.Fatalf("filtered notification was delivered: %q", got)
	}
}

func TestSenderIncludesOptionalDecisionEvent(t *testing.T) {
	app, output := fakeApp(t)
	sender := New(config.Notifications{Enabled: true, Switch: true, Helper: app}, "", nil)
	sender.SendEvent("switch", "group", "a to b", "event-opaque-123")
	sender.Close()

	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	got := string(contents)
	for _, expected := range []string{"--event-id", "event-opaque-123"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("helper output %q does not contain %q", got, expected)
		}
	}
}

func TestDisabledSenderDoesNotRunHelper(t *testing.T) {
	app, output := fakeApp(t)
	sender := New(config.Notifications{Enabled: false, Summary: true, Switch: true, Helper: app}, "", nil)
	sender.Send("switch", "title", "body")
	sender.Close()
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("disabled sender created output: %v", err)
	}
}

func TestDeliveryIncludesAbsoluteDashboardConfigWithoutChangingHelperArguments(t *testing.T) {
	output := filepath.Join(t.TempDir(), "context")
	app := fakeAppWithScript(t, "#!/bin/sh\nprintf '%s\\n' \"$MI_GUARD_NOTIFICATION_CONFIG\" \"$#\" > "+shellQuote(output)+"\n")
	path := filepath.Join("example config", "guard.yaml")
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	sender := New(config.Notifications{Enabled: true, Switch: true, Helper: app}, path, nil)
	sender.Send("switch", "group", "node")
	sender.Close()
	data, err := os.ReadFile(output)
	if err != nil || string(data) != absolute+"\n7\n" {
		t.Fatalf("helper context %q, %v", data, err)
	}
}

func TestSuccessfulDeliveryLogsHistoryDiagnostic(t *testing.T) {
	app := fakeAppWithScript(t, "#!/bin/sh\nprintf '%s\n' 'notification delivered but history was not saved' >&2\n")
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	sender := New(config.Notifications{Enabled: true, Switch: true, Helper: app}, "", logger)
	sender.Send("switch", "group", "node")
	sender.Close()
	if got := logs.String(); !strings.Contains(got, "history was not saved") {
		t.Fatalf("delivery diagnostic was not logged: %q", got)
	}
}

func TestAuthorizeUsesExplicitHelperMode(t *testing.T) {
	app, output := fakeApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := Authorize(ctx, app); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(contents)) != "--authorize" {
		t.Fatalf("authorize arguments = %q", contents)
	}
}

func TestAuthorizeIncludesHelperDiagnostic(t *testing.T) {
	app := fakeAppWithScript(t, "#!/bin/sh\nprintf '%s\\n' 'notification permission is denied' >&2\nexit 4\n")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := Authorize(ctx, app)
	if err == nil || !strings.Contains(err.Error(), "notification permission is denied") {
		t.Fatalf("authorize error = %v", err)
	}
}

func TestAuthorizationNoticeStreamsBeforeHelperExit(t *testing.T) {
	app := fakeAppWithScript(t, "#!/bin/sh\nprintf '%s\\n' 'Waiting for notification permission in the macOS system prompt...'\nexec sleep 10\n")
	executable, err := resolveHelperPath(app)
	if err != nil {
		t.Fatal(err)
	}
	read, write := io.Pipe()
	defer read.Close()
	defer write.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- runAuthorizationCommand(ctx, executable, write)
	}()

	line := make(chan string, 1)
	go func() {
		value, _ := bufio.NewReader(read).ReadString('\n')
		line <- value
	}()
	select {
	case got := <-line:
		if !strings.Contains(got, "Waiting for notification permission") {
			t.Fatalf("authorization notice = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("authorization notice was not streamed while helper was running")
	}

	cancel()
	select {
	case err := <-result:
		if err != context.Canceled {
			t.Fatalf("authorization cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("authorization helper did not stop after cancellation")
	}
}

func TestResolveHelperAutoUsesHomebrewLayoutThroughBinSymlink(t *testing.T) {
	root := t.TempDir()
	cellar := filepath.Join(root, "Cellar", "mi-guard", "HEAD-abc")
	executable := filepath.Join(cellar, "bin", "mihomo-guard")
	writeExecutable(t, executable)
	app := filepath.Join(cellar, "libexec", helperApp)
	helper := writeApp(t, app)
	writeApp(t, filepath.Join(root, "home", "Applications", helperApp))

	prefixExecutable := filepath.Join(root, "bin", "mihomo-guard")
	if err := os.MkdirAll(filepath.Dir(prefixExecutable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, prefixExecutable); err != nil {
		t.Fatal(err)
	}

	got, err := resolveHelperFrom(config.AutoNotificationHelper, prefixExecutable, filepath.Join(root, "home"))
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, got, helper)
}

func TestResolveHelperAutoSupportsBuildAndMakeLayouts(t *testing.T) {
	t.Run("source build", func(t *testing.T) {
		root := t.TempDir()
		executable := filepath.Join(root, "build", "mihomo-guard")
		writeExecutable(t, executable)
		helper := writeApp(t, filepath.Join(root, "build", helperApp))
		got, err := resolveHelperFrom(config.AutoNotificationHelper, executable, filepath.Join(root, "home"))
		if err != nil {
			t.Fatal(err)
		}
		assertSameFile(t, got, helper)
	})

	t.Run("make install", func(t *testing.T) {
		root := t.TempDir()
		home := filepath.Join(root, "home")
		helper := writeApp(t, filepath.Join(home, "Applications", helperApp))
		got, err := resolveHelperFrom(config.AutoNotificationHelper, "", home)
		if err != nil {
			t.Fatal(err)
		}
		assertSameFile(t, got, helper)
	})
}

func TestResolveHelperConfiguredPathTakesPrecedence(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "Cellar", "mi-guard", "1.0", "bin", "mihomo-guard")
	writeExecutable(t, executable)
	writeApp(t, filepath.Join(filepath.Dir(executable), "..", "libexec", helperApp))
	configured := filepath.Join(root, "custom.app")
	want := writeApp(t, configured)

	got, err := resolveHelperFrom(configured, "", "")
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, got, want)

	missing := filepath.Join(root, "missing-custom.app")
	if _, err := resolveHelperFrom(missing, executable, filepath.Join(root, "home")); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing configured helper should not fall back, got %v", err)
	}
}

func TestResolveHelperLegacyGeneratedDefaultFallsBackOnlyWhenMissing(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	legacy := filepath.Join(home, "Applications", helperApp)
	executable := filepath.Join(root, "Cellar", "mi-guard", "1.0", "bin", "mihomo-guard")
	writeExecutable(t, executable)
	brewHelper := writeApp(t, filepath.Join(filepath.Dir(executable), "..", "libexec", helperApp))

	got, err := resolveHelperFrom(legacy, executable, home)
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, got, brewHelper)

	legacyHelper := writeApp(t, legacy)
	got, err = resolveHelperFrom(legacy, executable, home)
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, got, legacyHelper)
}

func fakeApp(t *testing.T) (string, string) {
	t.Helper()
	output := filepath.Join(t.TempDir(), "arguments.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(output) + "\n"
	return fakeAppWithScript(t, script), output
}

func fakeAppWithScript(t *testing.T, script string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "mi-guard.app")
	executable := filepath.Join(root, "Contents", "MacOS", helperExecutable)
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func writeApp(t *testing.T, root string) string {
	t.Helper()
	executable := filepath.Join(filepath.Clean(root), "Contents", "MacOS", helperExecutable)
	writeExecutable(t, executable)
	return executable
}

func assertSameFile(t *testing.T, got, want string) {
	t.Helper()
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("resolved helper = %q, want %q", got, want)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
