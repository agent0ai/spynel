package orchestrator

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/frdel/spynel/internal/config"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func Create(cfg config.Config, routeName, title, body string) (string, error) {
	var route *config.Route
	for i := range cfg.Orchestrator.Routes {
		if cfg.Orchestrator.Routes[i].Name == routeName {
			route = &cfg.Orchestrator.Routes[i]
			break
		}
	}
	if route == nil {
		return "", fmt.Errorf("orchestrator route %q is not configured", routeName)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "", errors.New("title is required")
	}
	now := time.Now().UTC()
	id := routeName + "-" + now.Format("20060102-150405") + "-" + randomSuffix()
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(slug) > 64 {
		slug = slug[:64]
	}
	if slug == "" {
		slug = "item"
	}
	status := filepath.Base(filepath.Clean(route.Source))
	front := map[string]any{
		"id": id, "title": title, "status": status,
		"created_at": now.Format(time.RFC3339), "updated_at": now.Format(time.RFC3339), "attempt": 0,
	}
	if routeName == "goals" {
		front["next_review_at"] = now.Format(time.RFC3339)
	}
	if strings.TrimSpace(body) == "" {
		body = "# " + title + "\n\n" + title + "\n\n## Progress\n\n- Created by Spynel.\n"
	}
	document := Document{FrontMatter: front, Body: body}
	directory := cfg.Resolve(route.Source)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(directory, slug+"-"+randomSuffix()+".md")
	if err := WriteDocument(path, document); err != nil {
		return "", err
	}
	return path, nil
}

func randomSuffix() string {
	data := make([]byte, 3)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	}
	return hex.EncodeToString(data)
}
