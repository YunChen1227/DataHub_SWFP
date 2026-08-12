// Package mask redacts PII for audit logs and structured logging.
package mask

// CreditCode keeps the first 4 and last 4 chars of a unified social credit code.
func CreditCode(s string) string { return keepEnds(s, 4, 4) }

// Mobile keeps the first 3 and last 4 chars (e.g. 138****1009).
func Mobile(s string) string { return keepEnds(s, 3, 4) }

func keepEnds(s string, head, tail int) string {
	r := []rune(s)
	if len(r) <= head+tail {
		if len(r) == 0 {
			return ""
		}
		out := string(r[0])
		for i := 1; i < len(r); i++ {
			out += "*"
		}
		return out
	}
	out := string(r[:head])
	for i := head; i < len(r)-tail; i++ {
		out += "*"
	}
	out += string(r[len(r)-tail:])
	return out
}
