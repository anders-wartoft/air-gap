package properties

import "strings"

// StripComment trims a properties line and removes a # comment at the start
// of the line or immediately after a space or tab. Embedded hashes are literal.
func StripComment(line string) string {
	line = strings.TrimSpace(line)
	for i := 0; i < len(line); i++ {
		if line[i] == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
			return strings.TrimSpace(line[:i])
		}
	}
	return line
}
