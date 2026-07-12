package tailer

import (
	"regexp"
	"strings"
)

// levelPattern matches a recognizable log-level token anywhere in a line
// (e.g. "level=warn", "[ERROR]", "INFO:"). Deliberately simple — a
// best-effort heuristic, not a log-format parser.
var levelPattern = regexp.MustCompile(`(?i)\b(FATAL|ERROR|WARNING|WARN|INFO|DEBUG|TRACE)\b`)

// detectLevel returns the normalized, uppercased level token found in
// line, or "" if none is recognized. "WARNING" normalizes to "WARN".
func detectLevel(line string) string {
	m := levelPattern.FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	level := strings.ToUpper(m[1])
	if level == "WARNING" {
		level = "WARN"
	}
	return level
}
