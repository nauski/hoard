package agent

import (
	"fmt"
	"strings"
)

func trimNewline(s string) string { return strings.Trim(s, "\r\n") }
func trimSpace(s string) string   { return strings.TrimSpace(s) }

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
