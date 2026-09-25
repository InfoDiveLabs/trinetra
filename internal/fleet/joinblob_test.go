package fleet

import (
	"strings"
	"testing"
)

func TestJoinBlobRoundTrip(t *testing.T) {
	in := JoinInfo{URL: "https://mon.example.com:9443", Token: "swt_abc", Pin: "sha256:xyz"}
	s := EncodeJoin(in)
	if !strings.HasPrefix(s, "swj1_") || strings.ContainsAny(s, " \n+/=") {
		t.Fatalf("blob not shell/url safe: %q", s)
	}
	out, err := DecodeJoin(s)
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip = %+v", out)
	}
}

func TestJoinBlobRejectsBadInput(t *testing.T) {
	bad := []string{
		"",
		"swj2_abc",
		"swj1_!!!",
		EncodeJoin(JoinInfo{URL: "http://plain:9443", Token: "swt_a", Pin: "sha256:x"}),
		EncodeJoin(JoinInfo{URL: "https://m:9443", Token: "nope", Pin: "sha256:x"}),
		EncodeJoin(JoinInfo{URL: "https://m:9443", Token: "swt_a", Pin: "md5:x"}),
	}
	for _, s := range bad {
		if _, err := DecodeJoin(s); err == nil {
			t.Errorf("DecodeJoin(%q) accepted", s)
		}
	}
}
