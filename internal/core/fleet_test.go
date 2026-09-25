package core

import "testing"

func TestNodeFilterMatch(t *testing.T) {
	n := NodeSummary{ID: "abc123", Name: "web-1", Tags: []string{"prod", "web"}, State: "online", RemoteAddr: "10.0.0.5:4431"}
	cases := []struct {
		f    NodeFilter
		want bool
	}{
		{NodeFilter{}, true},
		{NodeFilter{Tag: "prod"}, true},
		{NodeFilter{Tag: "db"}, false},
		{NodeFilter{State: "down"}, false},
		{NodeFilter{Query: "WEB"}, true},
		{NodeFilter{Query: "10.0.0.5"}, true},
		{NodeFilter{Query: "abc"}, true},
		{NodeFilter{Query: "zzz"}, false},
	}
	for _, c := range cases {
		if got := c.f.Match(n); got != c.want {
			t.Errorf("%+v.Match = %v, want %v", c.f, got, c.want)
		}
	}
}
