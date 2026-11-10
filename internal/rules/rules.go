// Package rules matches HTTP requests against rules such as "POST /api/chat",
// "GET /v1/*" or "/health".
//
// A rule is an optional method, then a path. Without a method, it matches every method.
// A path ending in "*" matches every path that starts with what comes before the "*";
// any other path must match exactly. The query string is never part of the match.
package rules

import (
	"fmt"
	"strings"
)

// Rule is one parsed rule.
type Rule struct {
	Method string // upper case; "" for every method
	Path   string // exact path, or prefix when Prefix is true
	Prefix bool
}

// Parse reads one rule.
func Parse(s string) (Rule, error) {
	fields := strings.Fields(s)
	var r Rule
	switch len(fields) {
	case 1:
		r.Path = fields[0]
	case 2:
		r.Method, r.Path = strings.ToUpper(fields[0]), fields[1]
		for _, c := range r.Method {
			if c < 'A' || c > 'Z' {
				return Rule{}, fmt.Errorf("rule %q: %q is not an HTTP method", s, fields[0])
			}
		}
	default:
		return Rule{}, fmt.Errorf("rule %q: expected \"METHOD /path\" or \"/path\"", s)
	}
	if !strings.HasPrefix(r.Path, "/") {
		return Rule{}, fmt.Errorf("rule %q: the path must start with /", s)
	}
	if strings.HasSuffix(r.Path, "*") {
		r.Path, r.Prefix = strings.TrimSuffix(r.Path, "*"), true
	}
	if strings.Contains(r.Path, "*") {
		return Rule{}, fmt.Errorf("rule %q: * is only allowed at the end of the path", s)
	}
	return r, nil
}

// Match reports whether the rule covers a request.
func (r Rule) Match(method, path string) bool {
	if r.Method != "" && !strings.EqualFold(r.Method, method) {
		return false
	}
	if r.Prefix {
		return strings.HasPrefix(path, r.Path)
	}
	return path == r.Path
}

func (r Rule) String() string {
	s := r.Path
	if r.Prefix {
		s += "*"
	}
	if r.Method != "" {
		s = r.Method + " " + s
	}
	return s
}

// Set is a list of rules; it matches a request if any of its rules does.
type Set []Rule

// ParseSet reads a list of rules.
func ParseSet(list []string) (Set, error) {
	set := make(Set, 0, len(list))
	for _, s := range list {
		r, err := Parse(s)
		if err != nil {
			return nil, err
		}
		set = append(set, r)
	}
	return set, nil
}

// Match reports whether any rule covers the request.
func (s Set) Match(method, path string) bool {
	for _, r := range s {
		if r.Match(method, path) {
			return true
		}
	}
	return false
}
