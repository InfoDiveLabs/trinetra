package trinetra

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/InfoDiveLabs/trinetra/internal/config"
)

// TestTelegramInstallHint pins #106 part 2: install nudges the operator to set a token only
// on a genuinely unconfigured host; a host that already has a token.
func TestTelegramInstallHint(t *testing.T) {
	prev := cfgPath
	t.Cleanup(func() { cfgPath = prev })
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.json")

	save := func(token, chat string) {
		c := config.Default()
		c.Telegram.Token = token
		c.Telegram.ChatID = chat
		if err := saveCfg(c); err != nil {
			t.Fatal(err)
		}
	}

	if got := telegramInstallHint(); got != "" {
		t.Errorf("unconfigured hint = %q, want none", got)
	}

	save("123:abc", "")
	if got := telegramInstallHint(); strings.Contains(got, "set-token") || !strings.Contains(got, "/start") {
		t.Errorf("token-only hint = %q, want an enroll instruction", got)
	}

	save("123:abc", "555")
	if got := telegramInstallHint(); got != "" {
		t.Errorf("configured hint = %q, want none", got)
	}
}
