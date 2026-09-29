package config

import "testing"

func TestUpdateKeys(t *testing.T) {
	c := Default()
	if v, _ := c.Get("update.channel"); v != "stable" {
		t.Fatalf("default channel %q", v)
	}
	if v, _ := c.Get("update.check_interval"); v != "24h" {
		t.Fatalf("default interval %q", v)
	}
	for _, bad := range [][2]string{{"update.channel", "nightly"}, {"update.source", "s3"}, {"update.check_interval", "5m"}} {
		if err := c.Set(bad[0], bad[1]); err == nil {
			t.Errorf("Set(%s=%s) accepted", bad[0], bad[1])
		}
	}
	if err := c.Set("update.github_token", "ghp_x"); err != nil || c.Update.GitHubToken != "ghp_x" {
		t.Fatalf("token set: %v", err)
	}
	if !IsSecretKey("update.github_token") || IsSecretKey("update.channel") {
		t.Fatal("secret flag wrong")
	}
}
