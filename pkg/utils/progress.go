package utils

import (
	"fmt"
	"strings"
)

func ShowProgress(current, total int) {
	width := 50
	percentage := 0
	filled := 0
	if total > 0 {
		percentage = current * 100 / total
		filled = current * width / total
	}
	empty := width - filled

	fmt.Printf("\r%s[%s%s%s] %s%d%%%s",
		CYAN,
		strings.Repeat("█", filled),
		strings.Repeat("░", empty),
		CYAN,
		BOLD, percentage, NC)
}
