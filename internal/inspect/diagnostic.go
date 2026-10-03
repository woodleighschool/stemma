package inspect

import "fmt"

// FormatSubjectIDs bounds and quotes subject IDs for diagnostic messages.
func FormatSubjectIDs(ids []string) string {
	const maxIDs, maxRunes = 8, 160
	shown := make([]string, min(len(ids), maxIDs))
	for i := range shown {
		name := []rune(ids[i])
		shown[i] = ids[i]
		if len(name) > maxRunes {
			shown[i] = string(name[:maxRunes]) + "…"
		}
	}
	text := fmt.Sprintf("%q", shown)
	if len(ids) > len(shown) {
		text += fmt.Sprintf(" (%d more)", len(ids)-len(shown))
	}
	return text
}
