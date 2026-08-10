// Package theme owns Spynel's file-backed semantic color palettes.
package theme

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/agent0ai/spynel/internal/fsx"
	"gopkg.in/yaml.v3"
)

const DefaultName = "spynel"

var (
	validName  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	validColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

//go:embed themes/*.yaml
var builtinFS embed.FS

// builtinFiles defines the deliberate picker progression. The YAML assets are
// the only source of palette values and are also materialized as editable
// workspace files during initialization.
var builtinFiles = [...]string{
	"spynel.yaml", "hack-the-box.yaml", "github-colorblind-dark.yaml", "gruvbox-dark.yaml",
	"nord.yaml", "okabe-ito-dark.yaml", "gruvbox-light.yaml", "rose-pine-dawn.yaml",
	"tol-muted-light.yaml", "catppuccin-latte.yaml", "okabe-ito-light.yaml", "solarized-light.yaml",
}

// Colors names colors by their UI purpose so renderers never depend on a
// particular palette. Theme files may change every value independently.
type Colors struct {
	Background      string `yaml:"background"`
	Surface         string `yaml:"surface"`
	SurfaceElevated string `yaml:"surface_elevated"`
	SurfaceSelected string `yaml:"surface_selected"`
	Text            string `yaml:"text"`
	TextMuted       string `yaml:"text_muted"`
	Primary         string `yaml:"primary"`
	Secondary       string `yaml:"secondary"`
	Border          string `yaml:"border"`
	User            string `yaml:"user"`
	Success         string `yaml:"success"`
	Warning         string `yaml:"warning"`
	Error           string `yaml:"error"`
	Info            string `yaml:"info"`
	Code            string `yaml:"code"`
}

// Theme is one named palette loaded from .spynel/themes.
type Theme struct {
	Name               string `yaml:"name"`
	Description        string `yaml:"description"`
	Appearance         string `yaml:"appearance,omitempty"`
	ColorBlindFriendly bool   `yaml:"color_blind_friendly,omitempty"`
	Colors             Colors `yaml:"colors"`
}

// Default returns the primary built-in palette used when the selected theme is
// unavailable.
func Default() Theme {
	return Builtins()[0]
}

// Builtins loads the palettes embedded with the theme package. Invalid bundled
// assets are programmer errors and panic during use rather than silently
// weakening renderer invariants.
func Builtins() []Theme {
	values := make([]Theme, 0, len(builtinFiles))
	for _, name := range builtinFiles {
		data, err := builtinFS.ReadFile("themes/" + name)
		if err != nil {
			panic(fmt.Sprintf("read built-in theme %s: %v", name, err))
		}
		var value Theme
		if err := yaml.Unmarshal(data, &value); err != nil {
			panic(fmt.Sprintf("parse built-in theme %s: %v", name, err))
		}
		if err := value.Validate(); err != nil {
			panic(fmt.Sprintf("validate built-in theme %s: %v", name, err))
		}
		values = append(values, value)
	}
	return values
}

// StockNames returns the built-in picker progression. User themes follow these
// entries alphabetically.
func StockNames() []string {
	values := Builtins()
	names := make([]string, len(values))
	for index, value := range values {
		names[index] = value.Name
	}
	return names
}

// InstallBuiltins materializes missing built-in palettes as editable workspace
// files without replacing existing stock-named or custom themes.
func InstallBuiltins(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	for _, name := range builtinFiles {
		data, err := builtinFS.ReadFile("themes/" + name)
		if err != nil {
			return fmt.Errorf("read built-in theme %s: %w", name, err)
		}
		if err := fsx.AtomicCreateFile(filepath.Join(directory, name), data, 0o600); err != nil && !os.IsExist(err) {
			return fmt.Errorf("install built-in theme %s: %w", name, err)
		}
	}
	return nil
}

// Validate rejects incomplete palettes so a renderer can safely use every
// semantic value without quietly falling back to hard-coded colors.
func (t Theme) Validate() error {
	var problems []string
	if !validName.MatchString(t.Name) {
		problems = append(problems, "name must contain only letters, numbers, dots, underscores, or hyphens")
	}
	if strings.TrimSpace(t.Description) == "" {
		problems = append(problems, "description is required")
	}
	if t.Appearance != "" && t.Appearance != "light" && t.Appearance != "dark" {
		problems = append(problems, "appearance must be light or dark")
	}
	values := []struct {
		name  string
		value string
	}{
		{"background", t.Colors.Background}, {"surface", t.Colors.Surface}, {"surface_elevated", t.Colors.SurfaceElevated},
		{"surface_selected", t.Colors.SurfaceSelected}, {"text", t.Colors.Text}, {"text_muted", t.Colors.TextMuted},
		{"primary", t.Colors.Primary}, {"secondary", t.Colors.Secondary}, {"border", t.Colors.Border},
		{"user", t.Colors.User}, {"success", t.Colors.Success}, {"warning", t.Colors.Warning},
		{"error", t.Colors.Error}, {"info", t.Colors.Info}, {"code", t.Colors.Code},
	}
	for _, item := range values {
		if !validColor.MatchString(item.value) {
			problems = append(problems, "colors."+item.name+" must be a #RRGGBB color")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// LoadDir loads all YAML palettes in deterministic name order. A missing or
// empty directory is not an error and yields the built-in palettes.
func LoadDir(directory string) ([]Theme, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Builtins(), nil
		}
		return nil, fmt.Errorf("read themes: %w", err)
	}
	loaded := make([]Theme, 0, len(entries))
	seen := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if ext != ".yaml" && ext != ".yml" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil, fmt.Errorf("read theme %s: %w", entry.Name(), readErr)
		}
		var value Theme
		if parseErr := yaml.Unmarshal(data, &value); parseErr != nil {
			return nil, fmt.Errorf("parse theme %s: %w", entry.Name(), parseErr)
		}
		if validateErr := value.Validate(); validateErr != nil {
			return nil, fmt.Errorf("theme %s: %w", entry.Name(), validateErr)
		}
		key := strings.ToLower(value.Name)
		if previous, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate theme name %q in %s and %s", value.Name, previous, entry.Name())
		}
		seen[key] = entry.Name()
		loaded = append(loaded, value)
	}
	if len(loaded) == 0 {
		return Builtins(), nil
	}
	stockIndex := make(map[string]int, len(StockNames()))
	for index, name := range StockNames() {
		stockIndex[name] = index
	}
	sort.Slice(loaded, func(i, j int) bool {
		left, leftStock := stockIndex[strings.ToLower(loaded[i].Name)]
		right, rightStock := stockIndex[strings.ToLower(loaded[j].Name)]
		if leftStock && rightStock {
			return left < right
		}
		if leftStock != rightStock {
			return leftStock
		}
		return strings.ToLower(loaded[i].Name) < strings.ToLower(loaded[j].Name)
	})
	return loaded, nil
}

// Find returns a named palette using case-insensitive matching.
func Find(values []Theme, name string) (Theme, bool) {
	name = strings.TrimSpace(name)
	for _, value := range values {
		if strings.EqualFold(value.Name, name) {
			return value, true
		}
	}
	return Theme{}, false
}

// Selected resolves a configured name, falling back to the built-in default
// and then the first available theme when necessary.
func Selected(values []Theme, name string) Theme {
	if value, ok := Find(values, name); ok {
		return value
	}
	if value, ok := Find(values, DefaultName); ok {
		return value
	}
	if len(values) > 0 {
		return values[0]
	}
	return Default()
}
