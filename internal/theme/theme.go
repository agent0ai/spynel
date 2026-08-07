// Package theme owns Spynel's file-backed semantic color palettes.
package theme

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultName = "spynel"

var (
	validName  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
	validColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

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

// Default returns the embedded compatibility palette used when a workspace
// predates theme files or its selected theme disappears.
func Default() Theme {
	return Theme{
		Name:        DefaultName,
		Description: "Spynel's vivid midnight palette",
		Appearance:  "dark",
		Colors: Colors{
			Background: "#111927", Surface: "#171F2D", SurfaceElevated: "#1D2939", SurfaceSelected: "#2A3B50",
			Text: "#D4DCEB", TextMuted: "#8292AE", Primary: "#F472B6", Secondary: "#A7F3D0", Border: "#3D5276",
			User: "#60A5FA", Success: "#5EE6A8", Warning: "#F7C76B", Error: "#FB7185", Info: "#7DD3FC", Code: "#C4B5FD",
		},
	}
}

// Builtins returns the palettes available when an older workspace has no
// theme files yet. Fresh workspaces receive editable copies of the same twelve
// palettes, while this fallback makes upgrades useful without a forced init.
func Builtins() []Theme {
	return []Theme{
		Default(),
		{
			Name: "hack-the-box", Description: "Hack The Box chartreuse over deep green-navy surfaces", Appearance: "dark",
			Colors: Colors{
				Background: "#111927", Surface: "#171717", SurfaceElevated: "#1A2332", SurfaceSelected: "#26354A",
				Text: "#A4B1CD", TextMuted: "#7183A4", Primary: "#9FEF00", Secondary: "#D5FF80", Border: "#3D5276",
				User: "#5CB8FF", Success: "#9FEF00", Warning: "#FFCC66", Error: "#FF5F5F", Info: "#5CB8FF", Code: "#D5FF80",
			},
		},
		{
			Name: "github-colorblind-dark", Description: "Color-blind friendly: GitHub Primer blue/orange semantics on a dark canvas", Appearance: "dark", ColorBlindFriendly: true,
			Colors: Colors{
				Background: "#0D1117", Surface: "#151B23", SurfaceElevated: "#212830", SurfaceSelected: "#2F3742",
				Text: "#F0F6FC", TextMuted: "#9198A1", Primary: "#58A6FF", Secondary: "#D2A8FF", Border: "#656C76",
				User: "#79C0FF", Success: "#58A6FF", Warning: "#E3B341", Error: "#F0883E", Info: "#79C0FF", Code: "#D2A8FF",
			},
		},
		{
			Name: "gruvbox-dark", Description: "Warm yellow and sandy contrast from the original Gruvbox dark palette", Appearance: "dark",
			Colors: Colors{
				Background: "#282828", Surface: "#32302F", SurfaceElevated: "#3C3836", SurfaceSelected: "#504945",
				Text: "#EBDBB2", TextMuted: "#BDAE93", Primary: "#FFA85F", Secondary: "#D8DA66", Border: "#7C6F64",
				User: "#A9C6BA", Success: "#D8DA66", Warning: "#FABD2F", Error: "#FF7664", Info: "#A9C6BA", Code: "#E2A7B8",
			},
		},
		{
			Name: "nord", Description: "Arctic blue Polar Night and muted Frost accents from Nord", Appearance: "dark",
			Colors: Colors{
				Background: "#2E3440", Surface: "#3B4252", SurfaceElevated: "#434C5E", SurfaceSelected: "#4C566A",
				Text: "#ECEFF4", TextMuted: "#D8DEE9", Primary: "#A7D9E3", Secondary: "#D9BED3", Border: "#71809A",
				User: "#A6BED8", Success: "#B7CF9E", Warning: "#F0D39A", Error: "#E28A91", Info: "#A6BED8", Code: "#D9BED3",
			},
		},
		{
			Name: "okabe-ito-dark", Description: "Color-blind friendly: Okabe-Ito blue, orange, yellow, and purple on charcoal", Appearance: "dark", ColorBlindFriendly: true,
			Colors: Colors{
				Background: "#121212", Surface: "#1D1D1D", SurfaceElevated: "#292929", SurfaceSelected: "#383838",
				Text: "#F2F2F2", TextMuted: "#B5B5B5", Primary: "#56B4E9", Secondary: "#E69F00", Border: "#6E6E6E",
				User: "#72C5ED", Success: "#F0E442", Warning: "#E69F00", Error: "#FF8A52", Info: "#E39BC1", Code: "#F0E442",
			},
		},
		{
			Name: "gruvbox-light", Description: "Warm paper and earthy accents from the original Gruvbox light palette", Appearance: "light",
			Colors: Colors{
				Background: "#FBF1C7", Surface: "#F2E5BC", SurfaceElevated: "#EBDBB2", SurfaceSelected: "#D5C4A1",
				Text: "#3C3836", TextMuted: "#665C54", Primary: "#8D0005", Secondary: "#075F70", Border: "#7C6F64",
				User: "#075F70", Success: "#526B13", Warning: "#805100", Error: "#8D0005", Info: "#075F70", Code: "#7A365F",
			},
		},
		{
			Name: "rose-pine-dawn", Description: "Rosé Pine Dawn's warm porcelain canvas and rose, pine, and iris accents", Appearance: "light",
			Colors: Colors{
				Background: "#FAF4ED", Surface: "#FFFAF3", SurfaceElevated: "#F2E9E1", SurfaceSelected: "#DFDAD9",
				Text: "#575279", TextMuted: "#6F6A86", Primary: "#98445F", Secondary: "#286983", Border: "#8A849C",
				User: "#286983", Success: "#3A6B35", Warning: "#795000", Error: "#98445F", Info: "#286983", Code: "#6D4C87",
			},
		},
		{
			Name: "tol-muted-light", Description: "Color-blind friendly: Paul Tol muted categorical accents on a cool paper canvas", Appearance: "light", ColorBlindFriendly: true,
			Colors: Colors{
				Background: "#EAF3F7", Surface: "#DCEBF1", SurfaceElevated: "#CFDFE7", SurfaceSelected: "#BED2DC",
				Text: "#24343D", TextMuted: "#4D626C", Primary: "#5F2450", Secondary: "#332288", Border: "#607984",
				User: "#332288", Success: "#117733", Warning: "#665500", Error: "#882255", Info: "#315F78", Code: "#5F2450",
			},
		},
		{
			Name: "catppuccin-latte", Description: "Warm pastel accents on Catppuccin Latte surfaces", Appearance: "light",
			Colors: Colors{
				Background: "#EFF1F5", Surface: "#E6E9EF", SurfaceElevated: "#DCE0E8", SurfaceSelected: "#D8DCE4",
				Text: "#3C3F57", TextMuted: "#5C5F77", Primary: "#1856C9", Secondary: "#6F2CC5", Border: "#73768C",
				User: "#1454B8", Success: "#347522", Warning: "#825000", Error: "#B90B31", Info: "#006D82", Code: "#6F2CC5",
			},
		},
		{
			Name: "okabe-ito-light", Description: "Color-blind friendly: Okabe-Ito blue, orange, yellow, and purple on soft white", Appearance: "light", ColorBlindFriendly: true,
			Colors: Colors{
				Background: "#FCFCFA", Surface: "#F4F4F0", SurfaceElevated: "#EAEAE4", SurfaceSelected: "#DCEBF2",
				Text: "#171717", TextMuted: "#595959", Primary: "#00689F", Secondary: "#743F68", Border: "#767676",
				User: "#005C8E", Success: "#00689F", Warning: "#6A5500", Error: "#743F68", Info: "#005C8E", Code: "#743F68",
			},
		},
		{
			Name: "solarized-light", Description: "Ethan Schoonover's precision-balanced Solarized light palette", Appearance: "light",
			Colors: Colors{
				Background: "#FDF6E3", Surface: "#EEE8D5", SurfaceElevated: "#E5DECA", SurfaceSelected: "#EBE3CF",
				Text: "#4B626A", TextMuted: "#526A73", Primary: "#00689C", Secondary: "#575CA0", Border: "#71888F",
				User: "#00689C", Success: "#596B00", Warning: "#765800", Error: "#AA2530", Info: "#00736F", Code: "#575CA0",
			},
		},
	}
}

// StockNames is the deliberate picker progression for the stock collection.
// User themes follow these entries alphabetically.
func StockNames() []string {
	return []string{
		"spynel", "hack-the-box", "github-colorblind-dark", "gruvbox-dark",
		"nord", "okabe-ito-dark", "gruvbox-light", "rose-pine-dawn",
		"tol-muted-light", "catppuccin-latte", "okabe-ito-light", "solarized-light",
	}
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
// empty directory is not an error and yields the stock upgrade palettes.
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
