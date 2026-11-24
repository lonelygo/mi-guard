package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFollowGroupsLoadAndLegacyFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "guard.yaml")
	data := "groups: []\nselection:\n  follow_groups: {'🇹🇼 前置': '普通组'}\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	front := Group{Name: "🇹🇼 前置", Chain: "exit", Direct: "legacy"}
	if !c.Selection.SyncDirect || c.Selection.Follower(front) != "普通组" {
		t.Fatalf("loaded follow configuration = %+v", c.Selection)
	}
	front.Name = "other"
	if got := c.Selection.Follower(front); got != "legacy" {
		t.Fatalf("legacy follower = %q", got)
	}
	c.Selection.SyncDirect = false
	for _, name := range []string{"🇹🇼 前置", "other"} {
		front.Name = name
		if got := c.Selection.Follower(front); got != "" {
			t.Errorf("sync disabled, follower for %q = %q", name, got)
		}
	}
}

func TestFollowGroupsRejectReservedOrEmptyEndpoints(t *testing.T) {
	for _, mapping := range []string{"{A: A}", "{A: mi-guard-bench}", "{mi-guard-bench: A}", "{A: ''}", "{' ': B}"} {
		path := filepath.Join(t.TempDir(), "guard.yaml")
		if err := os.WriteFile(path, []byte("selection:\n  follow_groups: "+mapping+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "follow_groups") {
			t.Errorf("Load(%s) error = %v", mapping, err)
		}
	}
}
