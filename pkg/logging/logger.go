package logging

import (
	"fmt"
	"os"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

// Current time formatted as ISO 8601 with milliseconds
func Timestamp() string {
	return time.Now().Format("2006-01-02T15:04:05.000")
}

func LogInfo(format string, args ...any) {
	fmt.Printf("%s[%s][INFO]%s %s\n", utils.BLUE, Timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func LogSuccess(format string, args ...any) {
	fmt.Printf("%s[%s][✓]%s %s\n", utils.GREEN, Timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func LogWarning(format string, args ...any) {
	fmt.Printf("%s[%s][⚠]%s %s\n", utils.YELLOW, Timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func LogError(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s[%s][✗]%s %s\n", utils.RED, Timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func LogChaos(format string, args ...any) {
	fmt.Printf("%s[%s][CHAOS]%s %s\n", utils.MAGENTA, Timestamp(), utils.NC, fmt.Sprintf(format, args...))
}

func LogConfig(format string, args ...any) {
	fmt.Printf("%s[CONFIG]%s %s\n", utils.MAGENTA, utils.NC, fmt.Sprintf(format, args...))
}

func LogSimple(format string, args ...any) {
	timestamp := time.Now().Format("2006-01-02T15:04:05.000")
	fmt.Printf("[%s] %s\n", timestamp, fmt.Sprintf(format, args...))
}

func LogMetrics(format string, args ...any) {
	fmt.Printf("%s[%s][METRICS]%s %s\n", utils.CYAN, Timestamp(), utils.NC, fmt.Sprintf(format, args...))
}
