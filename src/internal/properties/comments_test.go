package properties

import "testing"

func TestStripComment(t *testing.T) {
	for _, test := range []struct {
		line, want string
	}{
		{"", ""},
		{" \t\r", ""},
		{"# comment", ""},
		{"  \t# privateKeyFiles=", ""},
		{"id=tcp#1", "id=tcp#1"},
		{"id=tcp  # TCP output", "id=tcp"},
		{"id=tcp\t# TCP output", "id=tcp"},
		{"id=tcp#1 # TCP output # more", "id=tcp#1"},
		{"id=#1", "id=#1"},
		{"id= # empty", "id="},
		{"url=https://example.test/#fragment # URL", "url=https://example.test/#fragment"},
		{"value=a=b#c # ignored=value", "value=a=b#c"},
		{"  id = tcp#1 \r", "id = tcp#1"},
	} {
		t.Run(test.line, func(t *testing.T) {
			if got := StripComment(test.line); got != test.want {
				t.Fatalf("StripComment(%q) = %q, want %q", test.line, got, test.want)
			}
		})
	}
}
