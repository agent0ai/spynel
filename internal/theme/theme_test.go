package theme

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultIsComplete(t *testing.T) {
	for _, value := range Builtins() {
		if err := value.Validate(); err != nil {
			t.Fatalf("built-in %q: %v", value.Name, err)
		}
	}
}

func TestBuiltinBrightnessAccessibilityAndContrastContract(t *testing.T) {
	values := Builtins()
	if len(values) != 12 {
		t.Fatalf("built-in theme count = %d, want 12", len(values))
	}
	counts := map[string]int{}
	accessible := map[string]int{}
	for _, value := range values {
		counts[value.Appearance]++
		background := relativeLuminance(value.Colors.Background)
		if value.Appearance == "dark" && background >= 0.18 {
			t.Errorf("%s declares dark with background luminance %.3f", value.Name, background)
		}
		if value.Appearance == "light" && background <= 0.70 {
			t.Errorf("%s declares light with background luminance %.3f", value.Name, background)
		}
		if value.ColorBlindFriendly {
			accessible[value.Appearance]++
			if !strings.Contains(strings.ToLower(value.Description), "color-blind friendly") {
				t.Errorf("%s accessibility is not explicit in its description", value.Name)
			}
			assertColorBlindStatusSeparation(t, value)
		}
		assertThemeContrast(t, value)
	}
	if counts["dark"] != 6 || counts["light"] != 6 {
		t.Fatalf("appearance counts = %#v, want six dark and six light", counts)
	}
	if accessible["dark"] != 2 || accessible["light"] != 2 {
		t.Fatalf("accessible appearance counts = %#v, want two dark and two light", accessible)
	}
}

func TestBuiltinOrderAndCollectionDistinctness(t *testing.T) {
	values := Builtins()
	want := []string{
		"spynel", "hack-the-box", "github-colorblind-dark", "gruvbox-dark",
		"nord", "okabe-ito-dark", "gruvbox-light", "rose-pine-dawn",
		"tol-muted-light", "catppuccin-latte", "okabe-ito-light", "solarized-light",
	}
	if len(values) != len(want) {
		t.Fatalf("built-in count = %d, ordered stock names = %d", len(values), len(want))
	}
	for index, value := range values {
		if value.Name != want[index] {
			t.Fatalf("built-in %d = %q, want %q", index, value.Name, want[index])
		}
		if StockNames()[index] != want[index] {
			t.Fatalf("stock name %d = %q, want %q", index, StockNames()[index], want[index])
		}
		wantAppearance := "dark"
		if index >= 6 {
			wantAppearance = "light"
		}
		if value.Appearance != wantAppearance {
			t.Fatalf("built-in %d (%s) appearance = %q, want %q", index, value.Name, value.Appearance, wantAppearance)
		}
	}
	if values[0] != Default() || values[1].Name != "hack-the-box" {
		t.Fatalf("required picker opening = %#v", values[:3])
	}
	for left := range values {
		for right := left + 1; right < len(values); right++ {
			if values[left].Appearance != values[right].Appearance {
				continue
			}
			if distance := semanticThemeDistance(values[left], values[right]); distance < 14 {
				t.Errorf("%s and %s semantic CIE76 mean distance %.1f is below 14", values[left].Name, values[right].Name, distance)
			}
		}
	}
}

func assertThemeContrast(t *testing.T, value Theme) {
	t.Helper()
	c := value.Colors
	textPairs := []struct{ name, foreground, background string }{
		{"text/background", c.Text, c.Background}, {"muted/background", c.TextMuted, c.Background},
		{"muted/surface", c.TextMuted, c.Surface},
		{"text/surface", c.Text, c.Surface}, {"text/elevated", c.Text, c.SurfaceElevated},
		{"text/selected", c.Text, c.SurfaceSelected}, {"primary/background", c.Primary, c.Background},
		{"secondary/background", c.Secondary, c.Background}, {"user/background", c.User, c.Background},
		{"success/background", c.Success, c.Background}, {"warning/background", c.Warning, c.Background},
		{"error/background", c.Error, c.Background}, {"info/background", c.Info, c.Background},
		{"code/elevated", c.Code, c.SurfaceElevated}, {"code/background", c.Code, c.Background},
	}
	for _, pair := range textPairs {
		if ratio := contrastRatio(pair.foreground, pair.background); ratio < 4.5 {
			t.Errorf("%s %s contrast %.2f:1 is below 4.5:1 (%s on %s)", value.Name, pair.name, ratio, pair.foreground, pair.background)
		}
	}
	controlPairs := []struct{ name, foreground, background string }{
		{"selected-control", c.Background, c.Primary}, {"selected-button", c.Primary, c.SurfaceSelected},
	}
	for _, pair := range controlPairs {
		if ratio := contrastRatio(pair.foreground, pair.background); ratio < 3 {
			t.Errorf("%s %s contrast %.2f:1 is below 3:1", value.Name, pair.name, ratio)
		}
	}
	if ratio := contrastRatio(c.Border, c.Background); ratio < 2 {
		t.Errorf("%s decorative border/background contrast %.2f:1 is below 2:1", value.Name, ratio)
	}
}

func contrastRatio(foreground, background string) float64 {
	a, b := relativeLuminance(foreground), relativeLuminance(background)
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

func relativeLuminance(value string) float64 {
	component := func(offset int) float64 {
		var raw uint64
		_, _ = fmt.Sscanf(value[offset:offset+2], "%02x", &raw)
		channel := float64(raw) / 255
		if channel <= 0.04045 {
			return channel / 12.92
		}
		return math.Pow((channel+0.055)/1.055, 2.4)
	}
	return 0.2126*component(1) + 0.7152*component(3) + 0.0722*component(5)
}

func semanticThemeDistance(left, right Theme) float64 {
	a := left.Colors
	b := right.Colors
	leftRoles := []string{a.Background, a.Surface, a.SurfaceSelected, a.Text, a.Primary, a.Secondary, a.User, a.Success, a.Warning, a.Error, a.Info, a.Code}
	rightRoles := []string{b.Background, b.Surface, b.SurfaceSelected, b.Text, b.Primary, b.Secondary, b.User, b.Success, b.Warning, b.Error, b.Info, b.Code}
	var total float64
	for index := range leftRoles {
		total += cie76(rgbToLab(leftRoles[index]), rgbToLab(rightRoles[index]))
	}
	return total / float64(len(leftRoles))
}

func assertColorBlindStatusSeparation(t *testing.T, value Theme) {
	t.Helper()
	for _, matrix := range []struct {
		name string
		m    [3][3]float64
	}{
		{"protanopia", [3][3]float64{{0.152286, 1.052583, -0.204868}, {0.114503, 0.786281, 0.099216}, {-0.003882, -0.048116, 1.051998}}},
		{"deuteranopia", [3][3]float64{{0.367322, 0.860646, -0.227968}, {0.280085, 0.672501, 0.047413}, {-0.011820, 0.042940, 0.968881}}},
	} {
		success := simulateCVD(value.Colors.Success, matrix.m)
		errorColor := simulateCVD(value.Colors.Error, matrix.m)
		warning := simulateCVD(value.Colors.Warning, matrix.m)
		if distance := cie76(rgbToLab(success), rgbToLab(errorColor)); distance < 18 {
			t.Errorf("%s success/error distance under %s = %.1f, want >= 18", value.Name, matrix.name, distance)
		}
		if distance := cie76(rgbToLab(warning), rgbToLab(errorColor)); distance < 12 {
			t.Errorf("%s warning/error distance under %s = %.1f, want >= 12", value.Name, matrix.name, distance)
		}
	}
}

func rgbToLab(value string) [3]float64 {
	var raw [3]uint64
	for index, offset := range []int{1, 3, 5} {
		_, _ = fmt.Sscanf(value[offset:offset+2], "%02x", &raw[index])
	}
	linear := func(channel uint64) float64 {
		value := float64(channel) / 255
		if value <= 0.04045 {
			return value / 12.92
		}
		return math.Pow((value+0.055)/1.055, 2.4)
	}
	r, g, b := linear(raw[0]), linear(raw[1]), linear(raw[2])
	x := (0.4124564*r + 0.3575761*g + 0.1804375*b) / 0.95047
	y := (0.2126729*r + 0.7151522*g + 0.0721750*b)
	z := (0.0193339*r + 0.1191920*g + 0.9503041*b) / 1.08883
	f := func(value float64) float64 {
		if value > 216.0/24389.0 {
			return math.Cbrt(value)
		}
		return (24389.0/27.0*value + 16) / 116
	}
	return [3]float64{116*f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))}
}

func cie76(left, right [3]float64) float64 {
	return math.Sqrt(math.Pow(left[0]-right[0], 2) + math.Pow(left[1]-right[1], 2) + math.Pow(left[2]-right[2], 2))
}

func simulateCVD(value string, matrix [3][3]float64) string {
	var rgb [3]uint64
	for index, offset := range []int{1, 3, 5} {
		_, _ = fmt.Sscanf(value[offset:offset+2], "%02x", &rgb[index])
	}
	converted := [3]int{}
	for row := range matrix {
		channel := matrix[row][0]*float64(rgb[0]) + matrix[row][1]*float64(rgb[1]) + matrix[row][2]*float64(rgb[2])
		converted[row] = int(math.Max(0, math.Min(255, math.Round(channel))))
	}
	return fmt.Sprintf("#%02X%02X%02X", converted[0], converted[1], converted[2])
}

func TestLoadDirSortsAndFindsThemes(t *testing.T) {
	directory := t.TempDir()
	writeTheme(t, directory, "z.yaml", strings.ReplaceAll(themeYAML, "THEME", "zeta"))
	writeTheme(t, directory, "a.yml", strings.ReplaceAll(themeYAML, "THEME", "Aurora"))
	values, err := LoadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 || values[0].Name != "Aurora" || values[1].Name != "zeta" {
		t.Fatalf("themes = %#v", values)
	}
	if found, ok := Find(values, "AURORA"); !ok || found.Description != "A test palette" {
		t.Fatalf("Find = %#v, %v", found, ok)
	}
}

func TestLoadDirRejectsIncompleteTheme(t *testing.T) {
	directory := t.TempDir()
	writeTheme(t, directory, "bad.yaml", "name: bad\ndescription: incomplete\ncolors:\n  background: '#000000'\n")
	if _, err := LoadDir(directory); err == nil || !strings.Contains(err.Error(), "colors.surface") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadDirFallsBackWhenMissing(t *testing.T) {
	values, err := LoadDir(filepath.Join(t.TempDir(), "missing"))
	want := StockNames()
	if err != nil || len(values) != len(want) {
		t.Fatalf("themes = %#v, err = %v", values, err)
	}
	for index, name := range want {
		if values[index].Name != name {
			t.Fatalf("theme %d = %q, want %q", index, values[index].Name, name)
		}
	}
}

func TestLoadDirFallsBackWhenEmpty(t *testing.T) {
	values, err := LoadDir(t.TempDir())
	if err != nil || len(values) != len(Builtins()) {
		t.Fatalf("themes = %#v, err = %v", values, err)
	}
}

func TestInstallBuiltinsUsesEmbeddedAssetsWithoutReplacingFiles(t *testing.T) {
	directory := t.TempDir()
	custom := []byte("user-owned palette\n")
	if err := os.WriteFile(filepath.Join(directory, "spynel.yaml"), custom, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := InstallBuiltins(directory); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(directory, "spynel.yaml")); err != nil || string(got) != string(custom) {
		t.Fatalf("existing palette = %q, %v", got, err)
	}
	for index, name := range StockNames()[1:] {
		data, err := os.ReadFile(filepath.Join(directory, name+".yaml"))
		if err != nil {
			t.Fatalf("read installed theme %q: %v", name, err)
		}
		var installed Theme
		if err := yaml.Unmarshal(data, &installed); err != nil {
			t.Fatalf("parse installed theme %q: %v", name, err)
		}
		if installed != Builtins()[index+1] {
			t.Fatalf("installed theme %q differs from runtime built-in", name)
		}
	}
}

func writeTheme(t *testing.T, directory, name, value string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(directory, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

const themeYAML = `name: THEME
description: A test palette
colors:
  background: "#111111"
  surface: "#222222"
  surface_elevated: "#333333"
  surface_selected: "#444444"
  text: "#eeeeee"
  text_muted: "#aaaaaa"
  primary: "#ff00ff"
  secondary: "#00ffff"
  border: "#555555"
  user: "#0088ff"
  success: "#00ff00"
  warning: "#ffff00"
  error: "#ff0000"
  info: "#00aaff"
  code: "#aa88ff"
`
