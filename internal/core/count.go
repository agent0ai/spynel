package core

import (
	"strconv"
	"strings"
)

// CompactCount formats constrained status indicators while keeping at most
// three visible numeric characters before a magnitude suffix. Exact counts
// remain the contract for detailed status, commands, and APIs.
func CompactCount(count int) string {
	if count <= 0 {
		return "0"
	}
	if count < 1000 {
		return strconv.Itoa(count)
	}
	suffixes := []string{"", "k", "m", "b", "t", "q", "e"}
	value := float64(count)
	suffix := 0
	for value >= 1000 && suffix < len(suffixes)-1 {
		value /= 1000
		suffix++
	}
	for {
		var rendered string
		if value < 10 {
			rendered = strings.TrimSuffix(strconv.FormatFloat(value, 'f', 1, 64), ".0")
		} else {
			rendered = strconv.FormatFloat(value, 'f', 0, 64)
		}
		if rendered != "1000" || suffix == len(suffixes)-1 {
			return rendered + suffixes[suffix]
		}
		value = 1
		suffix++
	}
}
