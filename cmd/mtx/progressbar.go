package main

import (
	"fmt"
	"strings"

	"mtx/internal/encode"
)

const barWidth = 30

// renderProgress draws one live-updating line: an overall "[i/N]" counter,
// the current file's name, a bar for how far its encode has gotten, and
// ffmpeg's own realtime-speed readout. It overwrites the previous line via
// a carriage return, so nothing is printed here until the encode finishes
// (that final line is left with report()).
func renderProgress(index, total int, name string, p encode.Progress) {
	filled := int(p.FractionDone * barWidth)
	bar := strings.Repeat("=", filled) + strings.Repeat(" ", barWidth-filled)
	fmt.Printf("\r[%d/%d] %-40s [%s] %3.0f%% %s   ",
		index, total, truncate(name, 40), bar, p.FractionDone*100, p.Speed)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// clearProgressLine wipes the in-progress bar before the final done/skipped/
// failed line for this file is printed underneath it.
func clearProgressLine() {
	fmt.Printf("\r%s\r", strings.Repeat(" ", 100))
}
