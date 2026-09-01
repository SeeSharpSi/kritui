// Package themes provides the five built-in color themes. Theme palettes are
// embedded Omarchy colors.toml files parsed once at startup; only the fixed
// built-in IDs can be resolved, so stored settings can never select arbitrary
// files.
package themes

import (
	"embed"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
)

//go:embed */colors.toml
var embedded embed.FS

// Theme is one built-in color theme ready for rendering.
type Theme struct {
	// ID is the slug persisted in settings, e.g. "nord".
	ID string
	// Label is the display name shown in the settings select.
	Label string
	// Mode is "light" or "dark" and drives the CSS color-scheme property.
	Mode string
	// Colors holds the raw Omarchy palette keys mapped to lowercase hex values.
	Colors map[string]string
	// Style is the inline CSS custom property declaration for the page root,
	// including color-scheme.
	Style string
}

// ErrUnknownTheme reports a theme ID outside the fixed built-in list.
var ErrUnknownTheme = errors.New("unknown theme")

var definitions = []struct {
	id    string
	label string
}{
	{"rose-pine", "Rose Pine Light"},
	{"rose-pine-dark", "Rose Pine Dark"},
	{"nord", "Nord"},
	{"tokyo-night", "Tokyo Night"},
	{"og", "OG"},
}

// cssVariables maps each CSS custom property to its canonical palette key and
// the palette key used as fallback when the canonical key is absent.
var cssVariables = []struct {
	css       string
	canonical string
	fallback  string
}{
	{"--color-background", "background", ""},
	{"--color-surface", "surface", "color0"},
	{"--color-surface-muted", "surface_muted", "color8"},
	{"--color-text", "text", "foreground"},
	{"--color-text-muted", "muted", "foreground"},
	{"--color-border", "border", "selection_background"},
	{"--color-border-strong", "border_strong", "color8"},
	{"--color-accent", "accent", ""},
	{"--color-on-accent", "on_accent", "background"},
	{"--color-gold", "gold", "color3"},
	{"--color-error", "error", "color1"},
}

var hexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// parsePalette parses the flat Omarchy colors.toml subset: one
// key = "value" pair per line, blank lines, whole-line and trailing comments.
func parsePalette(content []byte) (map[string]string, error) {
	palette := make(map[string]string)
	for number, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("colors.toml line %d: expected key = \"value\"", number+1)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !validPaletteKey(key) {
			return nil, fmt.Errorf("colors.toml line %d: invalid key %q", number+1, key)
		}
		if _, exists := palette[key]; exists {
			return nil, fmt.Errorf("colors.toml line %d: duplicate key %q", number+1, key)
		}
		parsed, err := parsePaletteString(value)
		if err != nil {
			return nil, fmt.Errorf("colors.toml line %d: %w", number+1, err)
		}
		palette[key] = parsed
	}
	return palette, nil
}

func validPaletteKey(key string) bool {
	if key == "" {
		return false
	}
	for _, character := range key {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func parsePaletteString(value string) (string, error) {
	if !strings.HasPrefix(value, `"`) {
		return "", fmt.Errorf("expected key = \"value\"")
	}

	escaped := false
	end := -1
	for index := 1; index < len(value); index++ {
		if escaped {
			escaped = false
			continue
		}
		if value[index] == '\\' {
			escaped = true
			continue
		}
		if value[index] == '"' {
			end = index
			break
		}
	}
	if end < 0 {
		return "", fmt.Errorf("unterminated string")
	}
	if trailing := strings.TrimSpace(value[end+1:]); trailing != "" && !strings.HasPrefix(trailing, "#") {
		return "", fmt.Errorf("unexpected trailing content")
	}
	parsed, err := strconv.Unquote(value[:end+1])
	if err != nil {
		return "", fmt.Errorf("invalid string: %w", err)
	}
	return parsed, nil
}

// buildTheme validates a parsed palette and maps it to CSS custom properties.
func buildTheme(id, label string, palette map[string]string) (Theme, error) {
	mode := palette["mode"]
	if mode != "light" && mode != "dark" {
		return Theme{}, fmt.Errorf("theme %s: mode must be \"light\" or \"dark\"", id)
	}
	colors := make(map[string]string, len(palette))
	for key, value := range palette {
		if key == "mode" {
			continue
		}
		if !hexColorPattern.MatchString(value) {
			return Theme{}, fmt.Errorf("theme %s: %s = %q is not a #rrggbb color", id, key, value)
		}
		colors[key] = strings.ToLower(value)
	}
	for _, key := range []string{"background", "foreground", "accent"} {
		if colors[key] == "" {
			return Theme{}, fmt.Errorf("theme %s: missing required color %q", id, key)
		}
	}
	style := make([]string, 0, len(cssVariables)+1)
	for _, variable := range cssVariables {
		value := colors[variable.canonical]
		if value == "" {
			fallback := variable.fallback
			if variable.css == "--color-surface-muted" && mode == "light" {
				fallback = "selection_background"
			}
			value = colors[fallback]
		}
		if value == "" {
			return Theme{}, fmt.Errorf("theme %s: cannot resolve color for %s", id, variable.css)
		}
		style = append(style, variable.css+":"+value)
	}
	style = append(style, "color-scheme:"+mode)
	return Theme{
		ID:     id,
		Label:  label,
		Mode:   mode,
		Colors: colors,
		Style:  strings.Join(style, ";"),
	}, nil
}

// builtInThemes parses and validates every embedded palette at package
// initialization, so broken built-in files fail at startup.
var builtInThemes = mustParseBuiltIns()

func mustParseBuiltIns() []Theme {
	parsed := make([]Theme, 0, len(definitions))
	for _, definition := range definitions {
		content, err := embedded.ReadFile(definition.id + "/colors.toml")
		if err != nil {
			panic(fmt.Sprintf("read built-in theme %s: %v", definition.id, err))
		}
		palette, err := parsePalette(content)
		if err != nil {
			panic(fmt.Sprintf("parse built-in theme %s: %v", definition.id, err))
		}
		theme, err := buildTheme(definition.id, definition.label, palette)
		if err != nil {
			panic(fmt.Sprintf("build built-in theme %s: %v", definition.id, err))
		}
		parsed = append(parsed, theme)
	}
	return parsed
}

// Options returns every built-in theme in fixed display order.
func Options() []Theme {
	options := make([]Theme, len(builtInThemes))
	for index, theme := range builtInThemes {
		options[index] = cloneTheme(theme)
	}
	return options
}

// ByID resolves one of the fixed built-in theme IDs. Unknown IDs return
// ErrUnknownTheme; there is no filesystem or path-based lookup.
func ByID(id string) (Theme, error) {
	for _, theme := range builtInThemes {
		if theme.ID == id {
			return cloneTheme(theme), nil
		}
	}
	return Theme{}, fmt.Errorf("%w: %q", ErrUnknownTheme, id)
}

// Default returns the theme used when nothing is configured.
func Default() Theme {
	return cloneTheme(builtInThemes[0])
}

func cloneTheme(theme Theme) Theme {
	theme.Colors = maps.Clone(theme.Colors)
	return theme
}
