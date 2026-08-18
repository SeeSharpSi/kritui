package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
)

const (
	gitCapability = "git"

	gitTreeDescription = `Lists a public Git repository as a compact Unix-style tree.
Use a path to inspect one directory and recursive only when descendants are needed. Repeated calls with the same URL and ref share one snapshot during the current response.`
	gitTreeParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Public GitHub, GitLab, or Bitbucket HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "revision": {"type": "string", "description": "Optional commit hash from git_log; defaults to selected ref"},
    "path": {"type": "string", "description": "Optional repository-relative directory"},
    "recursive": {"type": "boolean", "default": false, "description": "Include descendants"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Entries to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 100, "description": "Maximum source entries"}
  },
  "required": ["url"],
  "additionalProperties": false
}`

	gitReadDescription = `Reads lines from one tracked text file in a public Git repository.
Output is numbered plain text. Use offset and limit to page; use revision to read an older version returned by git_log.`
	gitReadParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Public GitHub, GitLab, or Bitbucket HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "revision": {"type": "string", "description": "Optional commit hash from git_log; defaults to selected ref"},
    "path": {"type": "string", "description": "Repository-relative file path"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Lines to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 400, "default": 200, "description": "Maximum lines"}
  },
  "required": ["url", "path"],
  "additionalProperties": false
}`

	gitSearchDescription = `Searches tracked text files in a public Git repository.
Returns compact grep-style path:line:text matches. Matching is literal by default; enable regex only when needed. Use path to narrow scope before increasing limit.`
	gitSearchParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Public GitHub, GitLab, or Bitbucket HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "revision": {"type": "string", "description": "Optional commit hash from git_log; defaults to selected ref"},
    "query": {"type": "string", "description": "Text or extended regular expression to find"},
    "path": {"type": "string", "description": "Optional repository-relative search scope"},
    "regex": {"type": "boolean", "default": false, "description": "Interpret query as an extended regular expression"},
    "case_sensitive": {"type": "boolean", "default": false, "description": "Use case-sensitive matching"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Matches to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "Maximum matches"}
  },
  "required": ["url", "query"],
  "additionalProperties": false
}`

	gitLogDescription = `Shows compact commit history for a public Git repository.
Returns hash, timestamp, author, and subject per line. Use path to inspect history relevant to one file or directory.`
	gitLogParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Public GitHub, GitLab, or Bitbucket HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "revision": {"type": "string", "description": "Optional commit hash at which history starts"},
    "path": {"type": "string", "description": "Optional repository-relative file or directory"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Commits to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "Maximum commits"}
  },
  "required": ["url"],
  "additionalProperties": false
}`

	gitDiffDescription = `Shows a compact unified diff between commits in a public Git repository.
Use hashes returned by git_log. Target defaults to the selected ref. Use path to limit output before paging a large diff.`
	gitDiffParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Public GitHub, GitLab, or Bitbucket HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "base": {"type": "string", "description": "Base commit hash from git_log"},
    "target": {"type": "string", "description": "Optional target commit hash from git_log; defaults to selected ref"},
    "path": {"type": "string", "description": "Optional repository-relative file or directory"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Diff lines to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 400, "default": 200, "description": "Maximum diff lines"}
  },
  "required": ["url", "base"],
  "additionalProperties": false
}`
)

type gitOperation uint8

const (
	gitTreeOperation gitOperation = iota
	gitReadOperation
	gitSearchOperation
	gitLogOperation
	gitDiffOperation
)

type gitTool struct {
	runtime   *gitRuntime
	operation gitOperation
}

type gitTreeArguments struct {
	URL       string `json:"url"`
	Ref       string `json:"ref"`
	Revision  string `json:"revision"`
	Path      string `json:"path"`
	Recursive *bool  `json:"recursive"`
	Offset    *int   `json:"offset"`
	Limit     *int   `json:"limit"`
}

type gitReadArguments struct {
	URL      string `json:"url"`
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
	Path     string `json:"path"`
	Offset   *int   `json:"offset"`
	Limit    *int   `json:"limit"`
}

type gitSearchArguments struct {
	URL           string `json:"url"`
	Ref           string `json:"ref"`
	Revision      string `json:"revision"`
	Query         string `json:"query"`
	Path          string `json:"path"`
	Regex         *bool  `json:"regex"`
	CaseSensitive *bool  `json:"case_sensitive"`
	Offset        *int   `json:"offset"`
	Limit         *int   `json:"limit"`
}

type gitLogArguments struct {
	URL      string `json:"url"`
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
	Path     string `json:"path"`
	Offset   *int   `json:"offset"`
	Limit    *int   `json:"limit"`
}

type gitDiffArguments struct {
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Base   string `json:"base"`
	Target string `json:"target"`
	Path   string `json:"path"`
	Offset *int   `json:"offset"`
	Limit  *int   `json:"limit"`
}

// NewGitTools creates focused, read-only public repository inspection tools.
// They share clones through a GitSession and appear as one selectable git
// capability.
func NewGitTools() []Tool {
	runtime := newGitRuntime()
	return []Tool{
		&gitTool{runtime: runtime, operation: gitTreeOperation},
		&gitTool{runtime: runtime, operation: gitReadOperation},
		&gitTool{runtime: runtime, operation: gitSearchOperation},
		&gitTool{runtime: runtime, operation: gitLogOperation},
		&gitTool{runtime: runtime, operation: gitDiffOperation},
	}
}

func (*gitTool) Capability() string {
	return gitCapability
}

func (t *gitTool) Definition() Definition {
	switch t.operation {
	case gitTreeOperation:
		return Definition{Name: "git_tree", Description: gitTreeDescription, Parameters: json.RawMessage(gitTreeParameters)}
	case gitReadOperation:
		return Definition{Name: "git_read", Description: gitReadDescription, Parameters: json.RawMessage(gitReadParameters)}
	case gitSearchOperation:
		return Definition{Name: "git_search", Description: gitSearchDescription, Parameters: json.RawMessage(gitSearchParameters)}
	case gitLogOperation:
		return Definition{Name: "git_log", Description: gitLogDescription, Parameters: json.RawMessage(gitLogParameters)}
	case gitDiffOperation:
		return Definition{Name: "git_diff", Description: gitDiffDescription, Parameters: json.RawMessage(gitDiffParameters)}
	default:
		panic("unknown git operation")
	}
}

func (t *gitTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	switch t.operation {
	case gitTreeOperation:
		return t.executeTree(ctx, arguments)
	case gitReadOperation:
		return t.executeRead(ctx, arguments)
	case gitSearchOperation:
		return t.executeSearch(ctx, arguments)
	case gitLogOperation:
		return t.executeLog(ctx, arguments)
	case gitDiffOperation:
		return t.executeDiff(ctx, arguments)
	default:
		return "", errors.New("git: unsupported operation")
	}
}

func (t *gitTool) executeTree(ctx context.Context, arguments json.RawMessage) (string, error) {
	var params gitTreeArguments
	if err := decodeGitArguments(arguments, &params); err != nil {
		return "", err
	}
	url, err := requiredGitURL(params.URL)
	if err != nil {
		return "", err
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	revision, err := optionalGitRevision(params.Revision)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 100, 500)
	if err != nil {
		return "", err
	}
	recursive := params.Recursive != nil && *params.Recursive

	return t.runtime.withRepository(ctx, url, params.Ref, func(repository *gitRepository) (string, error) {
		return gitTree(ctx, repository, revision, repositoryPath, recursive, offset, limit, gitMaxResultBytes)
	})
}

func (t *gitTool) executeRead(ctx context.Context, arguments json.RawMessage) (string, error) {
	var params gitReadArguments
	if err := decodeGitArguments(arguments, &params); err != nil {
		return "", err
	}
	url, err := requiredGitURL(params.URL)
	if err != nil {
		return "", err
	}
	repositoryPath, err := requiredGitPath(params.Path)
	if err != nil {
		return "", err
	}
	revision, err := optionalGitRevision(params.Revision)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 200, 400)
	if err != nil {
		return "", err
	}

	return t.runtime.withRepository(ctx, url, params.Ref, func(repository *gitRepository) (string, error) {
		return gitRead(ctx, repository, revision, repositoryPath, offset, limit, gitMaxResultBytes)
	})
}

func (t *gitTool) executeSearch(ctx context.Context, arguments json.RawMessage) (string, error) {
	var params gitSearchArguments
	if err := decodeGitArguments(arguments, &params); err != nil {
		return "", err
	}
	url, err := requiredGitURL(params.URL)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(params.Query) == "" {
		return "", errors.New("git: query is required and must not be empty")
	}
	if len(params.Query) > gitMaxQueryLength {
		return "", fmt.Errorf("git: query exceeds %d bytes", gitMaxQueryLength)
	}
	if hasUnsafeGitControl(params.Query) {
		return "", errors.New("git: query must not contain control characters")
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	revision, err := optionalGitRevision(params.Revision)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 20, 100)
	if err != nil {
		return "", err
	}
	regex := params.Regex != nil && *params.Regex
	caseSensitive := params.CaseSensitive != nil && *params.CaseSensitive

	return t.runtime.withRepository(ctx, url, params.Ref, func(repository *gitRepository) (string, error) {
		return gitSearch(ctx, repository, revision, params.Query, repositoryPath, regex, caseSensitive, offset, limit, gitMaxResultBytes)
	})
}

func (t *gitTool) executeLog(ctx context.Context, arguments json.RawMessage) (string, error) {
	var params gitLogArguments
	if err := decodeGitArguments(arguments, &params); err != nil {
		return "", err
	}
	url, err := requiredGitURL(params.URL)
	if err != nil {
		return "", err
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	revision, err := optionalGitRevision(params.Revision)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 20, 100)
	if err != nil {
		return "", err
	}

	return t.runtime.withRepository(ctx, url, params.Ref, func(repository *gitRepository) (string, error) {
		return gitLog(ctx, repository, revision, repositoryPath, offset, limit, gitMaxResultBytes)
	})
}

func (t *gitTool) executeDiff(ctx context.Context, arguments json.RawMessage) (string, error) {
	var params gitDiffArguments
	if err := decodeGitArguments(arguments, &params); err != nil {
		return "", err
	}
	url, err := requiredGitURL(params.URL)
	if err != nil {
		return "", err
	}
	base, err := requiredGitRevision(params.Base, "base")
	if err != nil {
		return "", err
	}
	target, err := optionalGitRevision(params.Target)
	if err != nil {
		return "", fmt.Errorf("git: target: %w", err)
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 200, 400)
	if err != nil {
		return "", err
	}

	return t.runtime.withRepository(ctx, url, params.Ref, func(repository *gitRepository) (string, error) {
		return gitDiff(ctx, repository, base, target, repositoryPath, offset, limit, gitMaxResultBytes)
	})
}

func decodeGitArguments(arguments json.RawMessage, target any) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &object); err != nil || object == nil {
		return errors.New("git: arguments must be a JSON object")
	}
	for name, value := range object {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("git: argument %q must not be null", name)
		}
	}

	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("git: invalid arguments: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("git: arguments must contain one JSON object")
	}
	return nil
}

func requiredGitURL(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errors.New("git: url is required")
	}
	return validateGitURL(value)
}

func optionalGitRevision(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !isHexCommit(value, 7, 40) {
		return "", errors.New("revision must be a 7 to 40 character hexadecimal commit hash")
	}
	return strings.ToLower(value), nil
}

func requiredGitRevision(value, name string) (string, error) {
	revision, err := optionalGitRevision(value)
	if err != nil {
		return "", fmt.Errorf("git: %s: %w", name, err)
	}
	if revision == "" {
		return "", fmt.Errorf("git: %s is required", name)
	}
	return revision, nil
}

func hasUnsafeGitControl(value string) bool {
	return strings.IndexFunc(value, func(character rune) bool {
		return character == 0 || character == '\n' || character == '\r' || unicode.IsControl(character)
	}) >= 0
}
