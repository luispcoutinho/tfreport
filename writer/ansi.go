package writer

import "github.com/luispcoutinho/tfreport/terraformstate"

// ansiBold is the ANSI escape sequence for bold text.
const ansiBold = "\033[1m"

// bold wraps s in ANSI bold and resets formatting after.
func bold(s string) string {
	if s == "" {
		return s
	}
	return ansiBold + s + terraformstate.ColorReset
}

// colorBold wraps s in ANSI color + bold and resets formatting after.
func colorBold(s, color string) string {
	if s == "" {
		return s
	}
	return color + ansiBold + s + terraformstate.ColorReset
}
