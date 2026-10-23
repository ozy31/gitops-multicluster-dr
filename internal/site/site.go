// Package site defines the two sites (regions) that drswitch moves traffic
// and workloads between, plus the transitional "both" flag state.
package site

import (
	"fmt"
	"strings"
)

// Site identifies one half of a primary/DR pair, or the transitional state
// in which both halves run full capacity.
type Site string

const (
	// Primary is the site that serves traffic in normal operation.
	Primary Site = "primary"
	// DR is the standby site that takes over during a failover.
	DR Site = "dr"
	// Both is a flag value only: while a switch is in progress both sites
	// run full capacity, so the old site keeps serving until DNS has moved.
	Both Site = "both"
)

// All returns the two real sites in a stable order.
func All() []Site { return []Site{Primary, DR} }

// Parse converts user input into a target Site (primary or dr).
func Parse(s string) (Site, error) {
	switch x := Site(strings.ToLower(strings.TrimSpace(s))); x {
	case Primary, DR:
		return x, nil
	default:
		return "", fmt.Errorf("unknown site %q (want %q or %q)", s, Primary, DR)
	}
}

// ParseFlag converts a stored flag value, which may also be "both".
func ParseFlag(s string) (Site, error) {
	if Site(strings.ToLower(strings.TrimSpace(s))) == Both {
		return Both, nil
	}
	return Parse(s)
}

// Valid reports whether s is a real site (not Both).
func (s Site) Valid() bool { return s == Primary || s == DR }

// Other returns the opposite site of the pair.
func (s Site) Other() Site {
	if s == Primary {
		return DR
	}
	return Primary
}

func (s Site) String() string { return string(s) }
