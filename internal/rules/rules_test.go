package rules

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		rule, method, path string
		want               bool
	}{
		{"POST /api/chat", "POST", "/api/chat", true},
		{"POST /api/chat", "post", "/api/chat", true},
		{"POST /api/chat", "GET", "/api/chat", false},
		{"POST /api/chat", "POST", "/api/chat/x", false},
		{"POST /v1/*", "POST", "/v1/chat/completions", true},
		{"POST /v1/*", "POST", "/v1/", true},
		{"POST /v1/*", "POST", "/v1", false},
		{"/health", "GET", "/health", true},
		{"/health", "HEAD", "/health", true},
		{"/*", "DELETE", "/bucket/key", true},
		{"get /*", "GET", "/", true},
	}
	for _, c := range cases {
		r, err := Parse(c.rule)
		if err != nil {
			t.Fatalf("%q: %v", c.rule, err)
		}
		if got := r.Match(c.method, c.path); got != c.want {
			t.Errorf("%q matches %s %s = %v, want %v", c.rule, c.method, c.path, got, c.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{"", "POST", "POST api/chat", "POST /a /b", "P0ST /a", "/a*b", "GET /*/x"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestString(t *testing.T) {
	for _, s := range []string{"POST /v1/*", "/health", "GET /api/tags"} {
		r, err := Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		if r.String() != s {
			t.Errorf("String() = %q, want %q", r.String(), s)
		}
	}
}

func TestSet(t *testing.T) {
	set, err := ParseSet([]string{"POST /api/generate", "POST /v1/*"})
	if err != nil {
		t.Fatal(err)
	}
	if !set.Match("POST", "/v1/embeddings") || !set.Match("POST", "/api/generate") || set.Match("GET", "/api/tags") {
		t.Fatal("set matches the wrong requests")
	}
	if Set(nil).Match("GET", "/") {
		t.Fatal("an empty set matches nothing")
	}
}
