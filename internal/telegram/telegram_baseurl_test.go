package telegram

import "testing"

// TestBaseURLDefault verifies that with TELEGRAM_BASE_URL unset the client
// targets the real Telegram API.
func TestBaseURLDefault(t *testing.T) {
	t.Setenv("TELEGRAM_BASE_URL", "")
	c := New("TESTTOKEN", "999")
	want := "https://api.telegram.org/botTESTTOKEN"
	if c.BaseURL != want {
		t.Fatalf("BaseURL = %q, want %q", c.BaseURL, want)
	}
}

// TestBaseURLOverride verifies that setting TELEGRAM_BASE_URL to a host
// redirects the client at the mock, appending "/bot<token>".
func TestBaseURLOverride(t *testing.T) {
	t.Setenv("TELEGRAM_BASE_URL", "http://mocktg:8080")
	c := New("TESTTOKEN", "999")
	want := "http://mocktg:8080/botTESTTOKEN"
	if c.BaseURL != want {
		t.Fatalf("BaseURL = %q, want %q", c.BaseURL, want)
	}
}
