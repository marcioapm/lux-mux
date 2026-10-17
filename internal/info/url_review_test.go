package info

import "testing"

func TestPlainURLSlashBoundary(t *testing.T) {
	for in, want := range map[string]string{
		"user:ab@cd:ef/gh@example.com:r.git": UnparseableURL,
		"user:ab/cd@example.com:o/r.git":     UnparseableURL,
		"user:ab/cd@example.com":             UnparseableURL,
		"user:ab/cd@x@example.com":           UnparseableURL,
		"git@host:o/x:y@z.git":               "git@host:o/x:y@z.git",
		"host:path@x:y":                      "x:y",
		"./u:p@r":                            "./u:p@r",
		"/srv/git/u:p@r.git":                 "/srv/git/u:p@r.git",
		"/srv/git/u:p@r:x.git":               "/srv/git/u:p@r:x.git",
		"C:/repo@x:y":                        "C:/repo@x:y",
		"c:/repo/u:p@r.git":                  "c:/repo/u:p@r.git",
		"git@host:path":                      "git@host:path",
		"git@host":                           "git@host",
	} {
		if got := PlainURL(in); got != want {
			t.Errorf("PlainURL(%q) = %q, want %q", in, got, want)
		}
	}
}
