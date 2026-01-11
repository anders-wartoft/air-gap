package inputfilter

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"sitia.nu/airgap/src/logging"
)

// FilterRule represents a single allow/deny rule with a compiled regex pattern
type FilterRule struct {
	Action  string         // "allow" or "deny"
	Pattern *regexp.Regexp // compiled regex pattern
	Raw     string         // original pattern string for logging
}

// InputFilter holds the configuration for input filtering
type InputFilter struct {
	Rules         []FilterRule
	DefaultAction string // "allow" or "deny"
	Timeout       time.Duration
}

// LoadFilterRules parses filter rules from a file path or inline string
// Format: action:regex_pattern (one per line, or comma-separated for inline)
func LoadFilterRules(rulesStr string, defaultAction string, timeout time.Duration) (*InputFilter, error) {
	if rulesStr == "" {
		return nil, nil // No filtering
	}

	var rules []FilterRule
	var lines []string

	// Check if it's a file path
	if strings.HasPrefix(rulesStr, "config/") || strings.HasPrefix(rulesStr, "/") || strings.HasSuffix(rulesStr, ".txt") {
		data, err := os.ReadFile(rulesStr)
		if err != nil {
			return nil, fmt.Errorf("failed to read filter rules file %s: %w", rulesStr, err)
		}
		lines = strings.Split(string(data), "\n")
	} else {
		// Inline rules, comma-separated
		lines = strings.Split(rulesStr, ",")
	}

	for i, line := range lines {
		line = strings.TrimSpace(line)

		// Skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid rule format at line %d: %s (expected format: action:pattern)", i+1, line)
		}

		action := strings.ToLower(strings.TrimSpace(parts[0]))
		if action != "allow" && action != "deny" {
			return nil, fmt.Errorf("invalid action '%s' at line %d (must be 'allow' or 'deny')", action, i+1)
		}

		pattern := strings.TrimSpace(parts[1])

		// Validate pattern length
		if len(pattern) > 1000 {
			return nil, fmt.Errorf("regex pattern too long at line %d (max 1000 chars)", i+1)
		}

		// Check for potentially dangerous patterns
		if err := checkDangerousPattern(pattern); err != nil {
			return nil, fmt.Errorf("potentially dangerous regex at line %d: %w", i+1, err)
		}

		// Compile regex
		regex, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid regex at line %d (%s): %w", i+1, pattern, err)
		}

		rules = append(rules, FilterRule{
			Action:  action,
			Pattern: regex,
			Raw:     pattern,
		})
	}

	// Validate default action
	defaultAction = strings.ToLower(strings.TrimSpace(defaultAction))
	if defaultAction == "" {
		defaultAction = "allow"
	}
	if defaultAction != "allow" && defaultAction != "deny" {
		return nil, fmt.Errorf("invalid filterDefaultAction '%s' (must be 'allow' or 'deny')", defaultAction)
	}

	return &InputFilter{
		Rules:         rules,
		DefaultAction: defaultAction,
		Timeout:       timeout,
	}, nil
}

// checkDangerousPattern checks for regex patterns that could cause catastrophic backtracking
func checkDangerousPattern(pattern string) error {
	dangerousPatterns := []string{
		`(\w+)*`, // Nested quantifiers
		`(a+)+`,  // Catastrophic backtracking
		`(.*)*`,  // Greedy + nested
		`(\w*)*`, // Another variant
		`(\d+)*`, // Nested digit quantifiers
		`(.+)+`,  // Another dangerous pattern
		`(\S+)*`, // Non-whitespace nested
	}

	for _, dangerous := range dangerousPatterns {
		if strings.Contains(pattern, dangerous) {
			return fmt.Errorf("pattern contains dangerous nested quantifier: %s", dangerous)
		}
	}
	return nil
}

// ShouldFilterOut determines if a payload should be filtered based on the rules
// Returns true if the payload should be filtered out (not sent)
func (f *InputFilter) ShouldFilterOut(payload []byte) (bool, error) {
	if f == nil || len(f.Rules) == 0 {
		return false, nil // No filtering
	}

	payloadStr := string(payload)

	// Apply rules in order, first match wins
	for i, rule := range f.Rules {
		matched, err := matchWithTimeout(rule.Pattern, payload, f.Timeout)
		if err != nil {
			return false, err // Timeout or error, fail open (don't filter)
		}
		if matched {
			// First match determines the action
			logging.Logger.Debugf("[InputFilter] Rule %d (%s) matched: %s", i+1, rule.Action, rule.Raw)
			logging.Logger.Debugf("[InputFilter] Payload (first 200 chars): %.200s", payloadStr)
			return rule.Action == "deny", nil
		}
	}

	// No match, use default action
	logging.Logger.Debugf("[InputFilter] No rules matched, using default action: %s", f.DefaultAction)
	logging.Logger.Debugf("[InputFilter] Payload (first 200 chars): %.200s", payloadStr)
	return f.DefaultAction == "deny", nil
}

// matchWithTimeout runs regex matching with a timeout to prevent ReDoS attacks
func matchWithTimeout(regex *regexp.Regexp, payload []byte, timeout time.Duration) (bool, error) {
	if timeout <= 0 {
		timeout = 100 * time.Millisecond // Default timeout
	}

	resultChan := make(chan bool, 1)
	errChan := make(chan error, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				errChan <- fmt.Errorf("panic during regex match: %v", r)
			}
		}()
		resultChan <- regex.Match(payload)
	}()

	select {
	case result := <-resultChan:
		return result, nil
	case err := <-errChan:
		return false, err
	case <-time.After(timeout):
		return false, fmt.Errorf("regex match timeout after %v", timeout)
	}
}

// GetRulesSummary returns a human-readable summary of loaded rules
func (f *InputFilter) GetRulesSummary() string {
	if f == nil || len(f.Rules) == 0 {
		return "No input filter rules loaded"
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Input filter rules (%d total, default: %s):\n", len(f.Rules), f.DefaultAction))
	for i, rule := range f.Rules {
		sb.WriteString(fmt.Sprintf("  %d. %s: %s\n", i+1, rule.Action, rule.Raw))
	}
	return sb.String()
}
