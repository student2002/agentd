// desensitize.go implements safe desensitization of log content.
//
// This file filters sensitive information from coding tool output to prevent
// credential leakage to the server, mainly including:
//   - DesensitizeLog: desensitizes full log content, used for batch log upload
//   - desensitizeOutputLine: desensitizes a single line of real-time output, used
//     for SSE streaming
//   - sensitivePatterns: precompiled regex patterns covering API keys, Bearer
//     tokens, AWS keys, and other formats
//
// Desensitized values are replaced with ***REDACTED***, keeping the key/value
// prefix for debugging.
package agent

import (
	"regexp"
)

// sensitivePatterns matches common key formats in log output, used for
// desensitization.
// Includes API keys, Bearer tokens, the generic key=value pattern, AWS keys,
// and hex token formats.
var sensitivePatterns = []*regexp.Regexp{
	// API key formats: sk-..., ghp_..., tm_..., etc.
	regexp.MustCompile(`(?i)(sk-[a-zA-Z0-9]{20,})`),
	regexp.MustCompile(`(?i)(ghp_[a-zA-Z0-9]{20,})`),
	regexp.MustCompile(`(?i)(tm_[a-zA-Z0-9_]{20,})`),
	// Bearer token format
	regexp.MustCompile(`(?i)(Bearer\s+)(\S+)`),
	// Generic key=value pattern
	regexp.MustCompile(`(?i)(api[_-]?key|token|password|secret|credential|auth[_-]?token|access[_-]?key|private[_-]?key)(\s*[=:]\s*)\S+`),
	// AWS-style key format
	regexp.MustCompile(`(?i)(AKIA[A-Z0-9]{16})`),
	// Generic hex token format (16+ hex chars after a known prefix)
	regexp.MustCompile(`(?i)(["']?[a-zA-Z0-9._-]*(?:key|token|secret|password|credential|pat)["']?\s*[=:]\s*["']?)([a-zA-Z0-9+/=_-]{16,})`),
}

// DesensitizeLog desensitizes log content, replacing API keys, tokens, passwords,
// and other credentials with ***REDACTED***.
// Used for the safety processing of log content before it is uploaded to the
// server.
//
// Parameters:
//   - content: the original log content
//
// Returns:
//   - string: the desensitized log content
func DesensitizeLog(content string) string {
	result := content
	for _, pat := range sensitivePatterns {
		result = pat.ReplaceAllStringFunc(result, func(match string) string {
			// For patterns with capture groups, keep the prefix and replace the value part
			groups := pat.FindStringSubmatch(match)
			if len(groups) >= 3 {
				return groups[1] + "***REDACTED***"
			}
			return "***REDACTED***"
		})
	}
	return result
}

// desensitizeOutputLine desensitizes a single output line, used for real-time
// output streams.
//
// Parameters:
//   - line: the single output line content
//
// Returns:
//   - string: the desensitized content
func desensitizeOutputLine(line string) string {
	for _, pat := range sensitivePatterns {
		line = pat.ReplaceAllStringFunc(line, func(match string) string {
			groups := pat.FindStringSubmatch(match)
			if len(groups) >= 3 {
				return groups[1] + "***REDACTED***"
			}
			return "***REDACTED***"
		})
	}
	return line
}
