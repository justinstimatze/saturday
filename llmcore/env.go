package llmcore

import (
	"bufio"
	"os"
	"strings"
)

// LoadDotEnv loads KEY=VALUE pairs from path into the process environment.
// Existing env vars take precedence (we never override). Lines starting
// with # or blank lines are skipped. Surrounding single/double quotes on
// values are stripped. Missing or unreadable file is a no-op.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		val = strings.Trim(val, `"'`)
		if os.Getenv(key) == "" {
			os.Setenv(key, val)
		}
	}
}
