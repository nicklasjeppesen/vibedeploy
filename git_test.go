package main

import "testing"

func TestParseRepo(t *testing.T) {
	ok := map[string]string{
		"nicklas/blog":                          "git@github.com:nicklas/blog.git",
		"https://github.com/nicklas/blog":       "git@github.com:nicklas/blog.git",
		"https://github.com/nicklas/blog.git/":  "git@github.com:nicklas/blog.git",
		"git@github.com:nicklas/blog.git":       "git@github.com:nicklas/blog.git",
		"ssh://git@gitlab.com/gruppe/under/app": "git@gitlab.com:gruppe/under/app.git",
	}
	for in, want := range ok {
		r, err := parseRepo(in)
		if err != nil || r.SSH() != want {
			t.Errorf("%q: fik %q, %v; ventede %q", in, r.SSH(), err, want)
		}
	}
	bad := []string{"", "file:///etc/passwd", "/etc/passwd", "http://github.com/a/b", "ext::sh -c id",
		"-oProxyCommand=id", "https://localhost/a/b", "https://user:pw@github.com/a/b", "https://github.com/a/../b",
		"https://github.com/a/b?x=1", "git@github.com:-x/b", "https://github.com:22/a/b", "github.com", "a/b/c d"}
	for _, in := range bad {
		if _, err := parseRepo(in); err == nil {
			t.Errorf("%q burde afvises", in)
		}
	}
	if u := (GitRepo{"github.com", "a/b"}).AddKeyURL(); u != "https://github.com/a/b/settings/keys/new" {
		t.Errorf("forkert GitHub-link: %s", u)
	}
}

func TestCheckBranch(t *testing.T) {
	for _, b := range []string{"", "main", "feature/ny-ting", "v1.2"} {
		if checkBranch(b) != nil {
			t.Errorf("%q burde være ok", b)
		}
	}
	for _, b := range []string{"--upload-pack=id", "-x", "a..b", "a b", "a;b"} {
		if checkBranch(b) == nil {
			t.Errorf("%q burde afvises", b)
		}
	}
}
