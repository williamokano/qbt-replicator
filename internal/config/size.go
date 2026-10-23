package config

import (
	"fmt"
	"strconv"
	"strings"
)

var sizeUnits = []struct {
	suffix string
	mult   int64
}{
	{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
	{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3},
	{"B", 1},
}

// ParseSize parses human sizes such as "1GiB", "500MB" or "1024".
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	if t == "" {
		return 0, nil
	}
	mult := int64(1)
	for _, u := range sizeUnits {
		if strings.HasSuffix(t, u.suffix) {
			mult = u.mult
			t = strings.TrimSpace(strings.TrimSuffix(t, u.suffix))
			break
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}
