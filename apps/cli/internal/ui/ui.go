// Package ui renders terminal output.
//
// Colour is applied only when the output is an interactive terminal. Writing
// escape codes into a pipe corrupts logs and breaks grep, which is why every
// styling helper routes through this package rather than hard-coding ANSI.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	reset   = "\033[0m"
	bold    = "\033[1m"
	dim     = "\033[2m"
	red     = "\033[31m"
	green   = "\033[32m"
	yellow  = "\033[33m"
	blue    = "\033[34m"
	magenta = "\033[35m"
	cyan    = "\033[36m"
	gray    = "\033[90m"
)

var colorEnabled = detectColor()

func detectColor() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// SetColor overrides colour detection.
func SetColor(enabled bool) { colorEnabled = enabled }

func paint(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + reset
}

func Bold(s string) string    { return paint(bold, s) }
func Dim(s string) string     { return paint(dim, s) }
func Red(s string) string     { return paint(red, s) }
func Green(s string) string   { return paint(green, s) }
func Yellow(s string) string  { return paint(yellow, s) }
func Blue(s string) string    { return paint(blue, s) }
func Magenta(s string) string { return paint(magenta, s) }
func Cyan(s string) string    { return paint(cyan, s) }
func Gray(s string) string    { return paint(gray, s) }

// Banner prints the product header.
func Banner(w io.Writer) {
	fmt.Fprintln(w, Bold(Magenta("  ▄ GENESIS"))+Bold(" AI FACTORY"))
	fmt.Fprintln(w, Gray("  autonomous software factory"))
	fmt.Fprintln(w)
}

// Success, Warn, Error and Info print a prefixed line.
func Success(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s %s\n", Green("✔"), fmt.Sprintf(format, args...))
}

func Warn(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s %s\n", Yellow("!"), fmt.Sprintf(format, args...))
}

func Error(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s %s\n", Red("✘"), fmt.Sprintf(format, args...))
}

func Info(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, "%s %s\n", Blue("›"), fmt.Sprintf(format, args...))
}

// Field prints an aligned key/value pair.
func Field(w io.Writer, key, value string) {
	fmt.Fprintf(w, "  %s %s\n", Gray(pad(key+":", 16)), value)
}

func pad(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// Table renders aligned columns with a header rule.
func Table(w io.Writer, headers []string, rows [][]string) {
	if len(rows) == 0 {
		fmt.Fprintln(w, Gray("  (nothing to show)"))
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	var header strings.Builder
	header.WriteString("  ")
	for i, h := range headers {
		header.WriteString(pad(strings.ToUpper(h), widths[i]+2))
	}
	fmt.Fprintln(w, Gray(strings.TrimRight(header.String(), " ")))

	for _, row := range rows {
		var line strings.Builder
		line.WriteString("  ")
		for i, cell := range row {
			if i < len(widths) {
				line.WriteString(pad(cell, widths[i]+2))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(line.String(), " "))
	}
}

// StatusBadge colours a lifecycle status.
func StatusBadge(status string) string {
	switch status {
	case "succeeded", "ready", "done", "active":
		return Green(status)
	case "running", "building", "working":
		return Cyan(status)
	case "failed", "error":
		return Red(status)
	case "canceled", "interrupted", "blocked":
		return Yellow(status)
	case "skipped":
		return Gray(status)
	}
	return Gray(status)
}

// LevelBadge colours an event level.
func LevelBadge(level string) string {
	switch level {
	case "error":
		return Red("ERR")
	case "warn":
		return Yellow("WRN")
	case "debug":
		return Gray("DBG")
	}
	return Blue("INF")
}

// ProgressBar renders a fixed-width bar.
func ProgressBar(fraction float64, width int) string {
	if fraction < 0 {
		fraction = 0
	}
	if fraction > 1 {
		fraction = 1
	}
	filled := int(fraction * float64(width))
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	return fmt.Sprintf("%s %3.0f%%", Cyan(bar), fraction*100)
}

// Duration renders a human-friendly elapsed time.
func Duration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}

// Bytes renders a byte count.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
