package adhoc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStableReleaseVersions(t *testing.T) {
	for version, stable := range map[string]bool{"v1.19.34": true, "1.20.0": true, "v1.20.0+darwin.1": true, "alpha-9f053c4": false, "v1.20.0-rc.1": false, "": false} {
		if got := releaseVersion.MatchString(version); got != stable {
			t.Errorf("release %q = %v", version, got)
		}
	}
}

func TestReadSubscriptionURL(t *testing.T) {
	const payload = "proxies: [{name: local, type: http, server: 127.0.0.1, port: 9}]"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "mihomo" {
			t.Errorf("subscription user agent %q", r.UserAgent())
		}
		if r.URL.Path == "/denied" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte(payload))
	}))
	defer srv.Close()
	b, id, err := readSource(context.Background(), srv.URL+"/provider?token=private-example")
	if err != nil || string(b) != payload || !strings.HasPrefix(id, "source-") {
		t.Fatalf("provider fetch: id=%s err=%v", id, err)
	}
	_, _, err = readSource(context.Background(), srv.URL+"/denied?token=private-example")
	if err == nil || strings.Contains(err.Error(), "private-example") {
		t.Fatalf("failed fetch leaked sensitive URL or succeeded: %v", err)
	}
}
