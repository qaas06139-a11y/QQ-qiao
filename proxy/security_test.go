package proxy

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSecureCompare(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"both empty", "", "", false},
		{"left empty", "", "secret", false},
		{"right empty", "secret", "", false},
		{"equal", "secret", "secret", true},
		{"different", "secret", "wrong", false},
		{"different length", "secret", "secrets", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := secureCompare(tc.a, tc.b); got != tc.want {
				t.Fatalf("secureCompare(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestSafeWebPathRejectsTraversal(t *testing.T) {
	cases := []string{
		"../config/config.go",
		"..\\config\\config.go",
		"a/../../etc/passwd",
		"/etc/passwd",
		"\\windows\\system32\\config",
		"foo/../bar",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			if got := safeWebPath(p); got != "" {
				t.Fatalf("safeWebPath(%q) = %q, expected empty (rejected)", p, got)
			}
		})
	}
}

func TestSafeWebPathAllowsCleanPaths(t *testing.T) {
	cases := []string{
		"index.html",
		"static/style.css",
		"sub/dir/file.js",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			got := safeWebPath(p)
			if got == "" {
				t.Fatalf("safeWebPath(%q) unexpectedly empty", p)
			}
			rootWithSep := webRoot + string(filepath.Separator)
			if !strings.HasPrefix(got, rootWithSep) {
				t.Fatalf("safeWebPath(%q) = %q escaped webRoot %q", p, got, webRoot)
			}
		})
	}
}
