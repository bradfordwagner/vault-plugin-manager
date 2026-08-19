package k8s

import "testing"

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/vault/plugins/foo-0.3.1": `'/vault/plugins/foo-0.3.1'`,
		"plain":                    `'plain'`,
		"":                         `''`,
		"has space":                `'has space'`,
		"has'quote":                `'has'\''quote'`,
		"; rm -rf /":               `'; rm -rf /'`,
		"$(whoami)":                `'$(whoami)'`,
		"a'b'c":                    `'a'\''b'\''c'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstField(t *testing.T) {
	cases := map[string]string{
		"abc123  /vault/plugins/foo\n": "abc123",
		"  leading hash rest":          "leading",
		"":                             "",
		"\n\n":                         "",
		"onlyone":                      "onlyone",
	}
	for in, want := range cases {
		if got := firstField(in); got != want {
			t.Errorf("firstField(%q) = %q, want %q", in, got, want)
		}
	}
}
