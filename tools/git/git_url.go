package git

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

const (
	gitMaxURLLength     = 2048
	gitMaxHostLength    = 512
	gitMaxPathLength    = 4096
	gitMaxQueryLength   = 512
	gitMaxRefLength     = 256
	gitMaxOffset        = 100000
	gitMaxTreeDepth     = 5
	gitMaxResultBytes   = 12 * 1024
	gitMaxErrorBytes    = 4 * 1024
	gitMaxInputLine     = 1024 * 1024
	gitMaxRepositoryMiB = 256
)

type parsedRepository struct {
	address string
	host    string
}

// validateGitURL checks the address syntax without applying a session
// allowlist. Host authorization happens when the session resolves the address.
func validateGitURL(value string) error {
	_, err := parseRepositoryURL(value)
	return err
}

func parseRepositoryURL(value string) (parsedRepository, error) {
	if hasUnsafeGitControl(value) {
		return parsedRepository{}, errors.New("git: repository URL must not contain control characters")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return parsedRepository{}, errors.New("git: url is required")
	}
	if len(value) > gitMaxURLLength {
		return parsedRepository{}, fmt.Errorf("git: repository URL exceeds %d bytes", gitMaxURLLength)
	}

	parsed, err := url.Parse(value)
	if err != nil || parsed == nil {
		return parsedRepository{}, errors.New("git: repository URL must be a fully formed HTTPS URL")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return parsedRepository{}, errors.New("git: repository URL must be a fully formed HTTPS URL")
	}
	if parsed.Opaque != "" {
		return parsedRepository{}, errors.New("git: repository URL must be a fully formed HTTPS URL")
	}
	if parsed.User != nil {
		return parsedRepository{}, errors.New("git: repository URL must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return parsedRepository{}, errors.New("git: repository URL must not contain a query or fragment")
	}
	if parsed.Hostname() == "" {
		return parsedRepository{}, errors.New("git: repository URL must be a fully formed HTTPS URL")
	}
	if strings.Trim(parsed.Path, "/") == "" {
		return parsedRepository{}, errors.New("git: repository URL must contain a repository path")
	}

	host, err := canonicalHost(parsed.Hostname(), parsed.Port())
	if err != nil {
		return parsedRepository{}, err
	}
	repositoryPath := parsed.EscapedPath()
	if strings.Trim(repositoryPath, "/") == "" {
		return parsedRepository{}, errors.New("git: repository URL must contain a repository path")
	}

	return parsedRepository{
		address: "https://" + host + repositoryPath,
		host:    host,
	}, nil
}

// canonicalHost normalizes a hostname and optional port. Port 443 is implied,
// IPv6 literals are bracketed, and hostnames are lowercased.
func canonicalHost(hostname, port string) (string, error) {
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if hostname == "" {
		return "", errors.New("git: repository host is required")
	}
	if len(hostname) > gitMaxHostLength {
		return "", fmt.Errorf("git: repository host exceeds %d bytes", gitMaxHostLength)
	}
	if hasUnsafeGitControl(hostname) || strings.ContainsAny(hostname, " \t/?#@[]\\") {
		return "", errors.New("git: repository host is invalid")
	}
	if strings.Contains(hostname, ":") && !isIPv6Literal(hostname) {
		return "", errors.New("git: repository host is invalid")
	}

	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", errors.New("git: repository port is invalid")
		}
		if number == 443 {
			port = ""
		} else {
			port = strconv.Itoa(number)
		}
	}
	if strings.Contains(hostname, ":") {
		hostname = "[" + hostname + "]"
	}
	if port != "" {
		return hostname + ":" + port, nil
	}
	return hostname, nil
}

func isIPv6Literal(hostname string) bool {
	if !strings.Contains(hostname, ":") {
		return false
	}
	for _, character := range hostname {
		if (character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') ||
			character == ':' || character == '.' || character == '%' {
			continue
		}
		return false
	}
	return true
}

// parseHostConfig normalizes one session allowlist entry.
func parseHostConfig(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("git: host is required")
	}
	if len(value) > gitMaxHostLength+16 {
		return "", errors.New("git: host is invalid")
	}
	if hasUnsafeGitControl(value) || strings.ContainsAny(value, " \t/?#@\\") {
		return "", errors.New("git: host is invalid")
	}
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return "", errors.New("git: host must not include a URL scheme")
	}

	candidate := value
	if strings.HasPrefix(candidate, "[") {
		// Already bracketed, optionally with a port.
	} else if strings.Count(candidate, ":") > 1 {
		// A bare IPv6 literal has no port unless it is bracketed.
		candidate = "[" + candidate + "]"
	}
	if strings.HasSuffix(candidate, ":") {
		return "", errors.New("git: host is invalid")
	}

	parsed, err := url.Parse("https://" + candidate)
	if err != nil || parsed == nil || parsed.Hostname() == "" {
		return "", errors.New("git: host is invalid")
	}
	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("git: host is invalid")
	}
	return canonicalHost(parsed.Hostname(), parsed.Port())
}

func validateGitRef(ref string) error {
	if ref == "" {
		return errors.New("git: ref must not be empty")
	}
	if len(ref) > gitMaxRefLength {
		return fmt.Errorf("git: ref exceeds %d bytes", gitMaxRefLength)
	}
	if hasUnsafeGitControl(ref) || strings.ContainsAny(ref, " \t~^:?*[\\") {
		return errors.New("git: ref is not a valid branch or tag")
	}
	if ref == "@" || strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") ||
		strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") {
		return errors.New("git: ref is not a valid branch or tag")
	}
	for component := range strings.SplitSeq(ref, "/") {
		if component == "" || component == "." || component == ".." ||
			strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return errors.New("git: ref is not a valid branch or tag")
		}
	}
	for _, character := range ref {
		if unicode.IsControl(character) || unicode.IsSpace(character) || character == 0x7f {
			return errors.New("git: ref is not a valid branch or tag")
		}
	}
	return nil
}

func validateGitCommitish(value, name string) error {
	if value == "" {
		return fmt.Errorf("git: %s is required", name)
	}
	if isHexCommit(value, 7, 40) {
		return nil
	}
	if validateGitRef(value) == nil {
		return nil
	}
	return fmt.Errorf("git: %s: must be a 7 to 40 character hexadecimal commit or a valid branch/tag name", name)
}

func requiredGitPath(value string) (string, error) {
	if value == "" {
		return "", errors.New("git: path is required")
	}
	cleaned, err := validateGitPath(value)
	if err != nil {
		return "", err
	}
	if cleaned == "" {
		return "", errors.New("git: path must identify a file")
	}
	return cleaned, nil
}

func optionalGitPath(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return validateGitPath(value)
}

func validateGitPath(repositoryPath string) (string, error) {
	if len(repositoryPath) > gitMaxPathLength {
		return "", fmt.Errorf("git: path exceeds %d bytes", gitMaxPathLength)
	}
	if hasUnsafeGitControl(repositoryPath) {
		return "", errors.New("git: path must not contain control characters")
	}
	if strings.HasPrefix(repositoryPath, "/") {
		return "", errors.New("git: path must be repository-relative")
	}
	for component := range strings.SplitSeq(repositoryPath, "/") {
		if component == ".." {
			return "", errors.New("git: path must not contain parent traversal")
		}
	}
	cleaned := cleanRepositoryPath(repositoryPath)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("git: path must not traverse outside the repository")
	}
	return cleaned, nil
}

func cleanRepositoryPath(value string) string {
	for strings.Contains(value, "//") {
		value = strings.ReplaceAll(value, "//", "/")
	}
	components := make([]string, 0, strings.Count(value, "/")+1)
	for _, component := range strings.Split(value, "/") {
		switch component {
		case "", ".":
			continue
		case "..":
			if len(components) == 0 {
				return ".."
			}
			components = components[:len(components)-1]
		default:
			components = append(components, component)
		}
	}
	if len(components) == 0 {
		return "."
	}
	return strings.Join(components, "/")
}

func gitPage(offsetValue, limitValue *int, defaultLimit, maxLimit int) (int, int, error) {
	offset := 0
	if offsetValue != nil {
		offset = *offsetValue
	}
	if offset < 0 || offset > gitMaxOffset {
		return 0, 0, fmt.Errorf("git: offset must be between 0 and %d", gitMaxOffset)
	}
	limit := defaultLimit
	if limitValue != nil {
		limit = *limitValue
	}
	if limit < 1 || limit > maxLimit {
		return 0, 0, fmt.Errorf("git: limit must be between 1 and %d", maxLimit)
	}
	return offset, limit, nil
}

func gitTreeDepth(value *int) (int, error) {
	depth := 1
	if value != nil {
		depth = *value
	}
	if depth < 1 || depth > gitMaxTreeDepth {
		return 0, fmt.Errorf("git: depth must be between 1 and %d", gitMaxTreeDepth)
	}
	return depth, nil
}

func isHexCommit(value string, minimum, maximum int) bool {
	if len(value) < minimum || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') && (character < 'A' || character > 'F') {
			return false
		}
	}
	return true
}

func hasUnsafeGitControl(value string) bool {
	return strings.IndexFunc(value, func(character rune) bool {
		return character == 0 || unicode.IsControl(character)
	}) >= 0
}

func literalGitPathspec(repositoryPath string) string {
	return ":(literal)" + repositoryPath
}
