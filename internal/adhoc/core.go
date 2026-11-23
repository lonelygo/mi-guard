// Package adhoc runs a private, temporary instance of the installed stable core
// for provider files that do not exist in the serving instance.
package adhoc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/lonelygo/mi-guard/internal/config"
	"github.com/lonelygo/mi-guard/internal/mihomo"
	"gopkg.in/yaml.v2"
)

var releaseVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(\+[0-9A-Za-z.-]+)?$`)

const sourceLimit = 16 << 20

// Start owns all temporary data until stop is called. User-supplied documents
// contribute only their proxies lists, never listeners, rules or controllers.
func Start(ctx context.Context, c config.Config, sources []string) (config.Config, func(), error) {
	dir, err := os.MkdirTemp("", "mi-guard-bench-")
	if err != nil {
		return c, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	providers := map[string]any{}
	var names []string
	for _, source := range sources {
		data, alias, err := readSource(ctx, source)
		if err != nil {
			return c, nil, err
		}
		if _, exists := providers[alias]; exists {
			continue
		}
		var meta struct {
			Proxies []struct {
				Name string `yaml:"name"`
			} `yaml:"proxies"`
		}
		if yaml.Unmarshal(data, &meta) != nil || len(meta.Proxies) == 0 {
			return c, nil, errors.New("source must be a Mihomo YAML document with a nonempty proxies list")
		}
		filename := alias + ".yaml"
		if err = os.WriteFile(filepath.Join(dir, filename), data, 0600); err != nil {
			return c, nil, err
		}
		provider := map[string]any{"type": "file", "path": "./" + filename}
		if len(sources) > 1 {
			provider["override"] = map[string]string{"additional-prefix": "[" + alias + "] "}
		}
		providers[alias] = provider
		names = append(names, alias)
	}
	if len(names) == 0 {
		return c, nil, errors.New("missing provider source")
	}
	controller, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return c, nil, err
	}
	defer controller.Close()
	ingress, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return c, nil, err
	}
	defer ingress.Close()
	random := make([]byte, 24)
	if _, err = rand.Read(random); err != nil {
		return c, nil, err
	}
	c.Mihomo.Secret = hex.EncodeToString(random)
	c.Mihomo.Controller = "http://" + controller.Addr().String()
	c.Mihomo.TestProxy = "http://" + ingress.Addr().String()
	c.Mihomo.TestGroup = "mi-guard-adhoc"
	c.Mihomo.ConfigPath = filepath.Join(dir, "core.yaml")
	c.Groups = nil
	doc := map[string]any{
		"external-controller": controller.Addr().String(), "secret": c.Mihomo.Secret,
		"allow-lan": false, "mode": "rule", "log-level": "silent", "ipv6": false,
		"dns": map[string]bool{"enable": false}, "tun": map[string]bool{"enable": false},
		"profile": map[string]bool{"store-selected": false}, "geo-auto-update": false,
		"listeners":       []any{map[string]any{"name": "bench", "type": "mixed", "listen": "127.0.0.1", "port": ingress.Addr().(*net.TCPAddr).Port, "proxy": c.Mihomo.TestGroup}},
		"proxy-groups":    []any{map[string]any{"name": c.Mihomo.TestGroup, "type": "select", "use": names}},
		"proxy-providers": providers, "rules": []string{"MATCH,REJECT"},
	}
	b, err := yaml.Marshal(doc)
	if err != nil {
		return c, nil, err
	}
	if err = os.WriteFile(c.Mihomo.ConfigPath, b, 0600); err != nil {
		return c, nil, err
	}
	_ = controller.Close()
	_ = ingress.Close()
	cmd := exec.Command(c.Mihomo.StableBinary, "-d", dir, "-f", c.Mihomo.ConfigPath)
	if err = cmd.Start(); err != nil {
		return c, nil, fmt.Errorf("start installed Mihomo core: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop := func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		cleanup()
	}
	client := mihomo.New(c.Mihomo.Controller, c.Mihomo.Secret)
	ready, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	for {
		if p, e := client.Proxy(ready, c.Mihomo.TestGroup); e == nil && p.Type == "Selector" && len(p.All) > 0 {
			version, verErr := client.Version(ready)
			if verErr != nil {
				stop()
				return c, nil, verErr
			}
			if !c.Mihomo.AllowPrerelease && !releaseVersion.MatchString(version) {
				stop()
				return c, nil, fmt.Errorf("stable_binary reports %q; select the installed stable core or explicitly set allow_prerelease", version)
			}
			ok = true
			return c, stop, nil
		}
		select {
		case <-ready.Done():
			stop()
			return c, nil, fmt.Errorf("temporary core startup: %w", ready.Err())
		case <-exited:
			return c, nil, errors.New("temporary core rejected the provider configuration")
		case <-tick.C:
		}
	}
}

func readSource(ctx context.Context, raw string) ([]byte, string, error) {
	var reader io.ReadCloser
	var err error
	identity := raw
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		u, e := url.Parse(raw)
		if e != nil || u.Hostname() == "" {
			return nil, "", errors.New("invalid subscription URL")
		}
		request, e := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
		if e != nil {
			return nil, "", errors.New("invalid subscription request")
		}
		request.Header.Set("User-Agent", "mihomo")
		resp, e := (&http.Client{Timeout: 20 * time.Second}).Do(request)
		if e != nil {
			return nil, "", errors.New("could not fetch subscription")
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, "", fmt.Errorf("subscription returned HTTP %d", resp.StatusCode)
		}
		reader = resp.Body
	} else {
		identity, err = filepath.Abs(raw)
		if err != nil {
			return nil, "", err
		}
		reader, err = os.Open(identity)
		if err != nil {
			return nil, "", err
		}
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, sourceLimit+1))
	if err != nil || len(data) > sourceLimit {
		return nil, "", errors.New("subscription read failed or source exceeds 16 MiB")
	}
	key := sha256.Sum256([]byte(identity))
	return data, fmt.Sprintf("source-%x", key[:6]), nil
}
