package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"seesharpsi/kritui/tools"
)

const (
	gitCapability = "git"

	gitTreeDescription = `Lists a Git repository as a compact Unicode tree.
Use path to inspect one directory. depth controls how many directory levels are expanded (1-5). Inspect any branch or tag with ref, or a commit returned by git_log with revision. If a path is a file, use git_read instead.`
	gitTreeParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Allowed HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "revision": {"type": "string", "description": "Optional 7-40 character commit hash from git_log"},
    "path": {"type": "string", "description": "Optional repository-relative directory"},
    "depth": {"type": "integer", "minimum": 1, "maximum": 5, "default": 1, "description": "Directory levels to expand"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 200, "description": "Maximum tree entries"}
  },
  "required": ["url"],
  "additionalProperties": false
}`

	gitReadDescription = `Reads numbered lines from one tracked text file in a Git repository.
Use offset and limit to page. Use ref for a branch or tag, or revision for an older commit from git_log. Binary files are rejected.`
	gitReadParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Allowed HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "revision": {"type": "string", "description": "Optional 7-40 character commit hash from git_log"},
    "path": {"type": "string", "description": "Repository-relative file path"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Lines to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 400, "default": 200, "description": "Maximum lines"}
  },
  "required": ["url", "path"],
  "additionalProperties": false
}`

	gitSearchDescription = `Searches tracked text files in a Git repository.
Returns compact grep-style path:line:text matches. Matching is literal by default; enable regex only when needed. Use path to narrow scope before increasing limit.`
	gitSearchParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Allowed HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "revision": {"type": "string", "description": "Optional 7-40 character commit hash from git_log"},
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

	gitLogDescription = `Shows compact commit history for a Git repository.
Returns hash, author date, author, and subject per line. Use path to inspect history relevant to one file or directory. Use ref to inspect another branch or tag.`
	gitLogParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Allowed HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag"},
    "revision": {"type": "string", "description": "Optional 7-40 character commit hash at which history starts"},
    "path": {"type": "string", "description": "Optional repository-relative file or directory"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Commits to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "Maximum commits"}
  },
  "required": ["url"],
  "additionalProperties": false
}`

	gitDiffDescription = `Shows a unified diff between two commits, branches, or tags in a Git repository.
base and target each accept a 7-40 character commit hash or a valid branch/tag name. target defaults to ref, then HEAD. Use path to limit output before paging a large diff.`
	gitDiffParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Allowed HTTPS repository URL"},
    "ref": {"type": "string", "description": "Optional branch or tag; also the default target"},
    "base": {"type": "string", "description": "Base commit hash, branch, or tag"},
    "target": {"type": "string", "description": "Optional target commit hash, branch, or tag; defaults to ref or HEAD"},
    "path": {"type": "string", "description": "Optional repository-relative file or directory"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Diff lines to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 400, "default": 200, "description": "Maximum diff lines"}
  },
  "required": ["url", "base"],
  "additionalProperties": false
}`

	gitBranchesDescription = `Lists branches in a Git repository, most recent commit first.
The clone default branch is marked with "* ". Set include_tags to append tags in tagged-commit date order. Use offset and limit to page.`
	gitBranchesParameters = `{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "Allowed HTTPS repository URL"},
    "include_tags": {"type": "boolean", "default": false, "description": "Append tags after branches"},
    "offset": {"type": "integer", "minimum": 0, "default": 0, "description": "Entries to skip"},
    "limit": {"type": "integer", "minimum": 1, "maximum": 500, "default": 100, "description": "Maximum entries"}
  },
  "required": ["url"],
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
	gitBranchesOperation
)

type gitTool struct {
	runtime   *gitRuntime
	operation gitOperation
}

type gitTreeArguments struct {
	URL      string `json:"url"`
	Ref      string `json:"ref"`
	Revision string `json:"revision"`
	Path     string `json:"path"`
	Depth    *int   `json:"depth"`
	Limit    *int   `json:"limit"`
}

type gitPathPageArguments struct {
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

type gitDiffArguments struct {
	URL    string `json:"url"`
	Ref    string `json:"ref"`
	Base   string `json:"base"`
	Target string `json:"target"`
	Path   string `json:"path"`
	Offset *int   `json:"offset"`
	Limit  *int   `json:"limit"`
}

type gitBranchesArguments struct {
	URL         string `json:"url"`
	IncludeTags *bool  `json:"include_tags"`
	Offset      *int   `json:"offset"`
	Limit       *int   `json:"limit"`
}

// NewGitTools creates focused, read-only repository inspection tools. They
// share one runtime and persistent per-account mirror cache and appear as one
// selectable git capability.
func NewGitTools() []tools.Tool {
	return newGitToolsWithRuntime(newGitRuntime())
}

func newGitToolsWithRuntime(runtime *gitRuntime) []tools.Tool {
	return []tools.Tool{
		&gitTool{runtime: runtime, operation: gitTreeOperation},
		&gitTool{runtime: runtime, operation: gitReadOperation},
		&gitTool{runtime: runtime, operation: gitSearchOperation},
		&gitTool{runtime: runtime, operation: gitLogOperation},
		&gitTool{runtime: runtime, operation: gitDiffOperation},
		&gitTool{runtime: runtime, operation: gitBranchesOperation},
	}
}

func (*gitTool) Capability() string {
	return gitCapability
}

func (t *gitTool) Definition() tools.Definition {
	switch t.operation {
	case gitTreeOperation:
		return tools.Definition{Name: "git_tree", Description: gitTreeDescription, Parameters: json.RawMessage(gitTreeParameters)}
	case gitReadOperation:
		return tools.Definition{Name: "git_read", Description: gitReadDescription, Parameters: json.RawMessage(gitReadParameters)}
	case gitSearchOperation:
		return tools.Definition{Name: "git_search", Description: gitSearchDescription, Parameters: json.RawMessage(gitSearchParameters)}
	case gitLogOperation:
		return tools.Definition{Name: "git_log", Description: gitLogDescription, Parameters: json.RawMessage(gitLogParameters)}
	case gitDiffOperation:
		return tools.Definition{Name: "git_diff", Description: gitDiffDescription, Parameters: json.RawMessage(gitDiffParameters)}
	case gitBranchesOperation:
		return tools.Definition{Name: "git_branches", Description: gitBranchesDescription, Parameters: json.RawMessage(gitBranchesParameters)}
	default:
		panic("unknown git operation")
	}
}

func (t *gitTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	var result string
	var err error
	switch t.operation {
	case gitTreeOperation:
		result, err = t.executeTree(ctx, arguments)
	case gitReadOperation:
		result, err = t.executeRead(ctx, arguments)
	case gitSearchOperation:
		result, err = t.executeSearch(ctx, arguments)
	case gitLogOperation:
		result, err = t.executeLog(ctx, arguments)
	case gitDiffOperation:
		result, err = t.executeDiff(ctx, arguments)
	case gitBranchesOperation:
		result, err = t.executeBranches(ctx, arguments)
	default:
		err = errors.New("git: unsupported operation")
	}
	if err != nil {
		return "", prefixGitError(err)
	}
	return result, nil
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
	ref, err := optionalGitRef(params.Ref)
	if err != nil {
		return "", err
	}
	revision, err := optionalGitRevision(params.Revision)
	if err != nil {
		return "", err
	}
	depth, err := gitTreeDepth(params.Depth)
	if err != nil {
		return "", err
	}
	_, limit, err := gitPage(nil, params.Limit, 200, 500)
	if err != nil {
		return "", err
	}

	return t.runtime.withRepository(ctx, url, func(repository *gitRepository) (string, error) {
		return gitTree(ctx, repository, ref, revision, repositoryPath, depth, limit, gitMaxResultBytes)
	})
}

func (t *gitTool) executeRead(ctx context.Context, arguments json.RawMessage) (string, error) {
	var params gitPathPageArguments
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
	ref, err := optionalGitRef(params.Ref)
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

	return t.runtime.withRepository(ctx, url, func(repository *gitRepository) (string, error) {
		return gitRead(ctx, repository, ref, revision, repositoryPath, offset, limit, gitMaxResultBytes)
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
	ref, err := optionalGitRef(params.Ref)
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

	return t.runtime.withRepository(ctx, url, func(repository *gitRepository) (string, error) {
		return gitSearch(ctx, repository, ref, revision, params.Query, repositoryPath, regex, caseSensitive, offset, limit, gitMaxResultBytes)
	})
}

func (t *gitTool) executeLog(ctx context.Context, arguments json.RawMessage) (string, error) {
	var params gitPathPageArguments
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
	ref, err := optionalGitRef(params.Ref)
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

	return t.runtime.withRepository(ctx, url, func(repository *gitRepository) (string, error) {
		return gitLog(ctx, repository, ref, revision, repositoryPath, offset, limit, gitMaxResultBytes)
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
	ref, err := optionalGitRef(params.Ref)
	if err != nil {
		return "", err
	}
	base, err := requiredGitCommitish(params.Base, "base")
	if err != nil {
		return "", err
	}
	target, err := optionalGitCommitish(params.Target, "target")
	if err != nil {
		return "", err
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 200, 400)
	if err != nil {
		return "", err
	}

	return t.runtime.withRepository(ctx, url, func(repository *gitRepository) (string, error) {
		return gitDiff(ctx, repository, ref, base, target, repositoryPath, offset, limit, gitMaxResultBytes)
	})
}

func (t *gitTool) executeBranches(ctx context.Context, arguments json.RawMessage) (string, error) {
	var params gitBranchesArguments
	if err := decodeGitArguments(arguments, &params); err != nil {
		return "", err
	}
	url, err := requiredGitURL(params.URL)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 100, 500)
	if err != nil {
		return "", err
	}
	includeTags := params.IncludeTags != nil && *params.IncludeTags

	return t.runtime.withRepository(ctx, url, func(repository *gitRepository) (string, error) {
		return gitBranches(ctx, repository, includeTags, offset, limit, gitMaxResultBytes)
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
	if err := validateGitURL(value); err != nil {
		return "", err
	}
	return strings.TrimSpace(value), nil
}

func optionalGitRef(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if err := validateGitRef(value); err != nil {
		return "", err
	}
	return value, nil
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

func requiredGitCommitish(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("git: %s is required", name)
	}
	if err := validateGitCommitish(value, name); err != nil {
		return "", err
	}
	if isHexCommit(value, 7, 40) {
		return strings.ToLower(value), nil
	}
	return value, nil
}

func optionalGitCommitish(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if err := validateGitCommitish(value, name); err != nil {
		return "", err
	}
	if isHexCommit(value, 7, 40) {
		return strings.ToLower(value), nil
	}
	return value, nil
}

func prefixGitError(err error) error {
	if err == nil {
		return nil
	}
	if strings.HasPrefix(err.Error(), "git:") {
		return err
	}
	return fmt.Errorf("git: %w", err)
}
