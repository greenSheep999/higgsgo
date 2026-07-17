package v1

import "fmt"

// fmtSscanf wraps fmt.Sscanf so images.go doesn't need to import fmt directly.
func fmtSscanf(s, format string, a ...any) (int, error) {
	return fmt.Sscanf(s, format, a...)
}
