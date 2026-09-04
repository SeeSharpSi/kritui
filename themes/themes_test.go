package themes

import (
	"errors"
	"maps"
	"strings"
	"testing"
)

func TestOptionsListsBuiltInThemesInFixedOrder(t *testing.T) {
	options := Options()
	wantIDs := []string{"rose-pine", "rose-pine-dark", "nord", "tokyo-night", "og", "forest-night"}
	wantLabels := []string{"Rose Pine Light", "Rose Pine Dark", "Nord", "Tokyo Night", "OG", "Forest Night"}
	wantModes := []string{"light", "dark", "dark", "dark", "dark", "dark"}
	if len(options) != len(wantIDs) {
		t.Fatalf("Options() returned %d themes, want %d", len(options), len(wantIDs))
	}
	for index, theme := range options {
		if theme.ID != wantIDs[index] || theme.Label != wantLabels[index] || theme.Mode != wantModes[index] {
			t.Errorf("theme %d = (%q, %q, %q), want (%q, %q, %q)",
				index, theme.ID, theme.Label, theme.Mode, wantIDs[index], wantLabels[index], wantModes[index])
		}
		if !strings.Contains(theme.Style, "color-scheme:"+theme.Mode) {
			t.Errorf("theme %s style %q missing color-scheme:%s", theme.ID, theme.Style, theme.Mode)
		}
		for _, variable := range []string{
			"--color-background", "--color-surface", "--color-surface-muted", "--color-text",
			"--color-text-muted", "--color-border", "--color-border-strong", "--color-accent",
			"--color-on-accent", "--color-gold", "--color-error",
		} {
			if !strings.Contains(theme.Style, variable+":#") {
				t.Errorf("theme %s style %q missing %s", theme.ID, theme.Style, variable)
			}
		}
		if !hexColorPattern.MatchString(theme.Colors["background"]) {
			t.Errorf("theme %s background = %q, want #rrggbb color", theme.ID, theme.Colors["background"])
		}
	}
}

func TestByIDResolvesBuiltInIDsOnly(t *testing.T) {
	nord, err := ByID("nord")
	if err != nil {
		t.Fatalf("ByID(nord) error: %v", err)
	}
	if nord.Label != "Nord" || nord.Mode != "dark" {
		t.Errorf("ByID(nord) = (%q, %q), want (Nord, dark)", nord.Label, nord.Mode)
	}
	rosePineDark, err := ByID("rose-pine-dark")
	if err != nil {
		t.Fatalf("ByID(rose-pine-dark) error: %v", err)
	}
	if rosePineDark.Label != "Rose Pine Dark" || rosePineDark.Mode != "dark" {
		t.Errorf("ByID(rose-pine-dark) = (%q, %q), want (Rose Pine Dark, dark)", rosePineDark.Label, rosePineDark.Mode)
	}
	if rosePineDark.Colors["background"] != "#191724" {
		t.Errorf("rose-pine-dark background = %q, want #191724", rosePineDark.Colors["background"])
	}
	og, err := ByID("og")
	if err != nil {
		t.Fatalf("ByID(og) error: %v", err)
	}
	if og.Label != "OG" || og.Mode != "dark" {
		t.Errorf("ByID(og) = (%q, %q), want (OG, dark)", og.Label, og.Mode)
	}
	if og.Colors["background"] != "#17151b" {
		t.Errorf("og background = %q, want #17151b", og.Colors["background"])
	}
	forestNight, err := ByID("forest-night")
	if err != nil {
		t.Fatalf("ByID(forest-night) error: %v", err)
	}
	if forestNight.Label != "Forest Night" || forestNight.Mode != "dark" {
		t.Errorf("ByID(forest-night) = (%q, %q), want (Forest Night, dark)", forestNight.Label, forestNight.Mode)
	}
	if forestNight.Colors["background"] != "#1a2125" {
		t.Errorf("forest-night background = %q, want #1a2125", forestNight.Colors["background"])
	}
	if forestNight.Colors["foreground"] != "#c9d1d9" {
		t.Errorf("forest-night foreground = %q, want #c9d1d9", forestNight.Colors["foreground"])
	}
	if forestNight.Colors["accent"] != "#8fbc8f" {
		t.Errorf("forest-night accent = %q, want #8fbc8f", forestNight.Colors["accent"])
	}
	if forestNight.Colors["muted"] != "#4a5568" {
		t.Errorf("forest-night muted = %q, want #4a5568", forestNight.Colors["muted"])
	}
	for _, id := range []string{"", "dracula", "../rose-pine", "Rose Pine", "rose-pine/colors.toml"} {
		if _, err := ByID(id); !errors.Is(err, ErrUnknownTheme) {
			t.Errorf("ByID(%q) error = %v, want ErrUnknownTheme", id, err)
		}
	}
}

func TestDefaultIsRosePine(t *testing.T) {
	if Default().ID != "rose-pine" {
		t.Errorf("Default() = %q, want rose-pine", Default().ID)
	}
}

func TestBuildThemeMapsCSSVariablesWithFallbacks(t *testing.T) {
	palette := map[string]string{
		"mode":                 "dark",
		"background":           "#112233",
		"foreground":           "#aabbcc",
		"accent":               "#445566",
		"color0":               "#010203",
		"color1":               "#ff0001",
		"color3":               "#fff003",
		"color8":               "#080808",
		"selection_background": "#090909",
	}
	theme, err := buildTheme("test", "Test", palette)
	if err != nil {
		t.Fatalf("buildTheme() error: %v", err)
	}
	want := strings.Join([]string{
		"--color-background:#112233",
		"--color-surface:#010203",
		"--color-surface-muted:#080808",
		"--color-text:#aabbcc",
		"--color-text-muted:#aabbcc",
		"--color-border:#090909",
		"--color-border-strong:#080808",
		"--color-accent:#445566",
		"--color-on-accent:#112233",
		"--color-gold:#fff003",
		"--color-error:#ff0001",
		"color-scheme:dark",
	}, ";")
	if theme.Style != want {
		t.Errorf("style = %q, want %q", theme.Style, want)
	}
}

func TestBuildThemePrefersCanonicalSemanticKeys(t *testing.T) {
	palette := map[string]string{
		"mode":          "light",
		"background":    "#112233",
		"foreground":    "#aabbcc",
		"accent":        "#445566",
		"color0":        "#010203",
		"color1":        "#ff0001",
		"color3":        "#fff003",
		"color8":        "#080808",
		"surface":       "#222222",
		"surface_muted": "#333333",
		"text":          "#444444",
		"muted":         "#aaaaaa",
		"border":        "#555555",
		"border_strong": "#666666",
		"on_accent":     "#777777",
		"gold":          "#888888",
		"error":         "#999999",
	}
	theme, err := buildTheme("test", "Test", palette)
	if err != nil {
		t.Fatalf("buildTheme() error: %v", err)
	}
	for _, variable := range []string{
		"--color-surface:#222222",
		"--color-surface-muted:#333333",
		"--color-text:#444444",
		"--color-text-muted:#aaaaaa",
		"--color-border:#555555",
		"--color-border-strong:#666666",
		"--color-on-accent:#777777",
		"--color-gold:#888888",
		"--color-error:#999999",
	} {
		if !strings.Contains(theme.Style, variable) {
			t.Errorf("style %q missing %s", theme.Style, variable)
		}
	}
}

func TestBuiltInThemesMapMutedForReadability(t *testing.T) {
	wantMuted := map[string]string{
		"rose-pine":      "#5f5a78",
		"rose-pine-dark": "#908caa",
		"nord":           "#c2cbd8",
		"tokyo-night":    "#b3bae0",
		"og":             "#9f9694",
		"forest-night":   "#4a5568",
	}
	for _, id := range []string{"rose-pine", "rose-pine-dark", "nord", "tokyo-night", "og", "forest-night"} {
		theme, err := ByID(id)
		if err != nil {
			t.Fatalf("ByID(%s) error: %v", id, err)
		}
		if got := theme.Colors["muted"]; got != wantMuted[id] {
			t.Errorf("theme %s muted = %q, want %q", id, got, wantMuted[id])
		}
		want := "--color-text-muted:" + wantMuted[id]
		if !strings.Contains(theme.Style, want) {
			t.Errorf("theme %s style %q missing %s", id, theme.Style, want)
		}
	}
}

func TestBuildThemeRejectsInvalidPalettes(t *testing.T) {
	valid := map[string]string{
		"mode":       "light",
		"background": "#112233",
		"foreground": "#aabbcc",
		"accent":     "#445566",
	}
	cases := []struct {
		name    string
		palette func() map[string]string
	}{
		{"missing mode", func() map[string]string { return clonePalette(valid, "mode", "") }},
		{"invalid mode", func() map[string]string { return clonePalette(valid, "mode", "auto") }},
		{"missing background", func() map[string]string { return clonePalette(valid, "background", "") }},
		{"missing foreground", func() map[string]string { return clonePalette(valid, "foreground", "") }},
		{"missing accent", func() map[string]string { return clonePalette(valid, "accent", "") }},
		{"invalid color", func() map[string]string { return clonePalette(valid, "background", "#12345") }},
		{"non-hex color", func() map[string]string { return clonePalette(valid, "accent", "teal") }},
		{"unresolvable fallback", func() map[string]string { return clonePalette(valid, "color3", "") }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := buildTheme("test", "Test", testCase.palette()); err == nil {
				t.Error("buildTheme() error = nil, want error")
			}
		})
	}
}

func clonePalette(base map[string]string, key, value string) map[string]string {
	palette := make(map[string]string, len(base)+1)
	for name, existing := range base {
		palette[name] = existing
	}
	if value == "" {
		delete(palette, key)
	} else {
		palette[key] = value
	}
	return palette
}

func TestParsePaletteParsesFlatTOML(t *testing.T) {
	content := []byte(`
# comment line

mode = "dark"
background = "#1a1b26" # trailing comment
foreground	=	"#a9b1d6"
`)
	palette, err := parsePalette(content)
	if err != nil {
		t.Fatalf("parsePalette() error: %v", err)
	}
	want := map[string]string{
		"mode":       "dark",
		"background": "#1a1b26",
		"foreground": "#a9b1d6",
	}
	if !maps.Equal(palette, want) {
		t.Errorf("palette = %v, want %v", palette, want)
	}
}

func TestParsePaletteRejectsMalformedLines(t *testing.T) {
	for _, content := range []string{
		"mode dark\n",
		"= \"dark\"\n",
		"mode = \"dark\n",
		"mode = dark\n",
	} {
		if _, err := parsePalette([]byte(content)); err == nil {
			t.Errorf("parsePalette(%q) error = nil, want error", content)
		}
	}
}
