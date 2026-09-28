package policy

import (
	"strings"
	"testing"
)

// Kyverno-style wildcards on qualified repo:tag: * crosses slashes
// (test/*:*), ? is one char, regex: opts into full regex. If this
// fails, plan add/remove and reap add pattern the wrong image sets.
func TestMatchImageGlob(t *testing.T) {
	if ok, err := MatchImage("*:10s", "test/busybox:10s"); err != nil || !ok {
		t.Errorf("glob *:10s vs test/busybox:10s = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, _ := MatchImage("*:10s", "test/busybox:10m"); ok {
		t.Error("glob *:10s matched test/busybox:10m, want no match")
	}
	if ok, err := MatchImage("test/*:*", "test/a/b:10s"); err != nil || !ok {
		t.Errorf("glob * must cross slashes: = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, _ := MatchImage("pile:v??", "pile:v01"); !ok {
		t.Error("glob ? did not match one char, want match")
	}
	if ok, _ := MatchImage("pile:v??", "pile:v001"); ok {
		t.Error("glob v?? matched v001, want anchored no-match")
	}
	if ok, _ := MatchImage("app:latest", "app:latest"); !ok {
		t.Error("exact literal did not match itself")
	}
	if _, err := MatchImage("", "app:latest"); err == nil {
		t.Error("empty pattern accepted, want refusal")
	}
}

func TestMatchImageRegex(t *testing.T) {
	if ok, err := MatchImage("regex:^scratch:", "scratch:10s"); err != nil || !ok {
		t.Errorf("regex: ^scratch: = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, _ := MatchImage("regex:^scratch:", "scratch/a:10s"); ok {
		t.Error("regex: ^scratch: matched repo scratch/a, want no match")
	}
	if ok, _ := MatchImage("regex::latest$", "app:latest"); !ok {
		t.Error("regex: :latest$ did not match app:latest")
	}
	if _, err := MatchImage("regex:([", "app:latest"); err == nil {
		t.Error("invalid regex: accepted, want refusal")
	}
	// Without the prefix the same text is a literal glob, never regex.
	if ok, _ := MatchImage("^scratch:", "^scratch:"); !ok {
		t.Error("glob metachars should match literally, want match")
	}
	if ok, _ := MatchImage("a.c", "abc"); ok {
		t.Error("glob dot matched any char, want literal dot")
	}
}

func TestParseExactImage(t *testing.T) {
	repo, tag, err := ParseExactImage("test/busybox:10s")
	if err != nil || repo != "test/busybox" || tag != "10s" {
		t.Errorf("parse = (%q, %q, %v), want (test/busybox, 10s, nil)", repo, tag, err)
	}
	for _, bad := range []string{"", "notag", "repo:", ":tag", "test/*:10s", "app:lat?st", "just*"} {
		if _, _, err := ParseExactImage(bad); err == nil {
			t.Errorf("ParseExactImage(%q) accepted, want refusal", bad)
		}
		if !strings.Contains(bad, ":") && bad != "" {
			continue
		}
	}
}
