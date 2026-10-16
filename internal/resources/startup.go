package resources

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// CheckStartup keeps startup consent separate from dependency installation grants.
// Check-only invocations report the restriction without authorizing a service.
func CheckStartup(allow, checkOnly bool) (bool, error) {
	limit, total, err := MemoryLimit()
	if err != nil {
		return false, fmt.Errorf("Discover startup memory allowance: %w", err)
	}
	_, stdinErr := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	_, stderrErr := unix.IoctlGetTermios(int(os.Stderr.Fd()), unix.TCGETS)
	return startupConsent(limit, total, allow, checkOnly, stdinErr == nil && stderrErr == nil, os.Stdin, os.Stderr)
}

func startupConsent(limit, total uint64, allow, checkOnly, interactive bool, input io.Reader, output io.Writer) (bool, error) {
	if total == 0 || limit == 0 || limit > total {
		return false, errors.New("Invalid startup memory allowance")
	}
	restricted := !atLeast(limit, total, 40)
	if !restricted {
		return false, nil
	}
	if _, err := fmt.Fprintf(output, "Pebril memory allowance is %d bytes of %d system bytes (%.2f%%), below 40%%. Restricted mode limits conversion concurrency to 1 and disables AVIF conversion.\n", limit, total, float64(limit)/float64(total)*100); err != nil {
		return false, err
	}
	if checkOnly || allow {
		return true, nil
	}
	if !interactive {
		return false, errors.New("Restricted startup requires --allow-low-memory for this invocation; dependency grants do not authorize it")
	}
	if _, err := fmt.Fprint(output, "Start in restricted mode? [y/N] "); err != nil {
		return false, err
	}
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil {
		return false, errors.New("Restricted startup was not authorized")
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	default:
		return false, errors.New("Restricted startup was not authorized")
	}
}
