package modelsdev

import (
	"net/url"
	"path"
	"sort"
	"strings"
)

var endpointSuffixes = []string{
	"/chat/completions",
	"/chat/messages",
	"/responses",
	"/messages",
}

// NormalizeEndpoint converts a configured completion URL into the base URL
// used for provider matching. It returns an empty string for invalid URLs.
func NormalizeEndpoint(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	host := strings.ToLower(parsed.Host)
	host = strings.TrimSuffix(host, ":80")
	host = strings.TrimSuffix(host, ":443")
	normalizedPath := strings.TrimRight(parsed.Path, "/")
	for _, suffix := range endpointSuffixes {
		if strings.HasSuffix(normalizedPath, suffix) {
			normalizedPath = strings.TrimSuffix(normalizedPath, suffix)
			break
		}
	}
	normalizedPath = strings.TrimRight(normalizedPath, "/")
	if normalizedPath != "" && !strings.HasPrefix(normalizedPath, "/") {
		normalizedPath = "/" + normalizedPath
	}
	return scheme + "://" + host + normalizedPath
}

func stripV1(endpoint string) string {
	return strings.TrimSuffix(endpoint, "/v1")
}

func endpointPathLength(endpoint string) int {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return 0
	}
	return len(path.Clean(parsed.Path))
}

func sortedModelIDs(provider Provider) []string {
	ids := make([]string, 0, len(provider.Models))
	for id := range provider.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
