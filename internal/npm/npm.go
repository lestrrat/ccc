// Package npm resolves package versions from the npm registry.
//
// ccc queries the registry over plain HTTP rather than shelling out to npm:
// the host is not required to have Node installed at all.
package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ClaudeCode is the package the image installs.
const ClaudeCode = "@anthropic-ai/claude-code"

const registry = "https://registry.npmjs.org"

// Latest returns the version tagged "latest" for the given package.
func Latest(ctx context.Context, pkg string) (string, error) {
	url := fmt.Sprintf("%s/%s/latest", registry, pkg)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build request for %s: %w", url, err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to query npm registry: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("npm registry returned %s for %s", res.Status, pkg)
	}

	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("failed to decode npm registry response: %w", err)
	}
	if body.Version == "" {
		return "", fmt.Errorf("npm registry returned no version for %s", pkg)
	}
	return body.Version, nil
}
