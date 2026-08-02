package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	gitCloneDepth                  = 50
	gitMaxRepositories             = 8
	gitRepositoryTTL               = 10 * time.Minute
	gitCloneTimeout                = 90 * time.Second
	gitOperationTimeout            = 20 * time.Second
	gitResourceCheckInterval       = 500 * time.Millisecond
	gitRepositoryMaxSize     int64 = 256 * 1024 * 1024
	gitFreeSpaceReserve      int64 = 512 * 1024 * 1024
	gitMaxResultBytes              = 24 * 1024
	gitMaxRepositoryOutput         = 96 * 1024
	gitMaxErrorBytes               = 4 * 1024
	gitMaxInputLineBytes           = 1024 * 1024
	gitMaxPathLength               = 4096
	gitMaxQueryLength              = 512
	gitMaxRefLength                = 256
	gitMaxOffset                   = 100000

	gitDescription = `Inspects a public Git repository using a temporary, read-only clone.
Call operation "open" first, then use its repo handle with tree, search, read, log, or diff. Call close when finished. Handles expire when the current completion ends or after 10 minutes.
Results are compact plain text, not JSON. Use tree and search to narrow scope before reading files. Pagination responses end with a next_offset marker when truncated.
Only public HTTPS repositories on github.com, gitlab.com, and bitbucket.org are accepted. This tool cannot modify repositories or run arbitrary Git commands.`
	gitParameters = `{
  "type": "object",
  "properties": {
    "operation": {
      "type": "string",
      "enum": ["open", "tree", "search", "read", "log", "diff", "close"],
      "description": "Repository operation to perform"
    },
    "url": {
      "type": "string",
      "description": "Public HTTPS repository URL; used only by open"
    },
    "ref": {
      "type": "string",
      "description": "Optional branch or tag for open"
    },
    "repo": {
      "type": "string",
      "description": "Temporary repository handle returned by open"
    },
    "path": {
      "type": "string",
      "description": "Optional repository-relative file or directory path"
    },
    "query": {
      "type": "string",
      "description": "Literal text to find; used only by search"
    },
    "case_sensitive": {
      "type": "boolean",
      "default": false,
      "description": "Whether search matching is case-sensitive"
    },
    "recursive": {
      "type": "boolean",
      "default": false,
      "description": "Whether tree should include descendants"
    },
    "base": {
      "type": "string",
      "description": "Base commit hash from log; used only by diff"
    },
    "offset": {
      "type": "integer",
      "minimum": 0,
      "default": 0,
      "description": "Number of entries, matches, lines, or commits to skip"
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 500,
      "description": "Maximum entries, matches, lines, or commits to return"
    }
  },
  "required": ["operation"],
  "additionalProperties": false
}`
)

var (
	errGitResourceLimit = errors.New("git: resource limit exceeded")
	errGitStopWalk      = errors.New("git: stop directory walk")

	gitAllowedHosts = map[string]struct{}{
		"bitbucket.org": {},
		"github.com":    {},
		"gitlab.com":    {},
	}
)

// GitTool provides bounded, read-only access to temporary public repository
// clones. Its zero value is ready for use and safe for concurrent calls.
type GitTool struct {
	mu           sync.Mutex
	repositories map[string]*gitRepository
	active       int
}

type gitRepository struct {
	mu            sync.Mutex
	root          string
	directory     string
	commit        string
	closed        bool
	timer         *time.Timer
	returnedBytes int
}

type gitArguments struct {
	Operation     string  `json:"operation"`
	URL           *string `json:"url"`
	Ref           *string `json:"ref"`
	Repo          *string `json:"repo"`
	Path          *string `json:"path"`
	Query         *string `json:"query"`
	CaseSensitive *bool   `json:"case_sensitive"`
	Recursive     *bool   `json:"recursive"`
	Base          *string `json:"base"`
	Offset        *int    `json:"offset"`
	Limit         *int    `json:"limit"`
	provided      map[string]struct{}
}

type gitCommandOptions struct {
	directory        string
	resourceRoot     string
	timeout          time.Duration
	allowExitCodeOne bool
}

type gitTreeEntry struct {
	name       string
	objectType string
}

type gitSearchMatch struct {
	path string
	line int
	text string
}

// NewGitTool creates a read-only Git repository inspection tool.
func NewGitTool() *GitTool {
	return &GitTool{repositories: make(map[string]*gitRepository)}
}

// Definition describes git repository inspection to an LLM.
func (*GitTool) Definition() Definition {
	return Definition{
		Name:        "git",
		Description: gitDescription,
		Parameters:  json.RawMessage(gitParameters),
	}
}

// Execute performs one bounded repository operation.
func (t *GitTool) Execute(ctx context.Context, arguments json.RawMessage) (string, error) {
	params, err := parseGitArguments(arguments)
	if err != nil {
		return "", err
	}

	switch params.Operation {
	case "open":
		if err := params.allowOnly("operation", "url", "ref"); err != nil {
			return "", err
		}
		return t.open(ctx, params)
	case "tree":
		if err := params.allowOnly("operation", "repo", "path", "recursive", "offset", "limit"); err != nil {
			return "", err
		}
		return t.tree(ctx, params)
	case "search":
		if err := params.allowOnly("operation", "repo", "query", "path", "case_sensitive", "offset", "limit"); err != nil {
			return "", err
		}
		return t.search(ctx, params)
	case "read":
		if err := params.allowOnly("operation", "repo", "path", "offset", "limit"); err != nil {
			return "", err
		}
		return t.read(ctx, params)
	case "log":
		if err := params.allowOnly("operation", "repo", "path", "offset", "limit"); err != nil {
			return "", err
		}
		return t.log(ctx, params)
	case "diff":
		if err := params.allowOnly("operation", "repo", "base", "path", "offset", "limit"); err != nil {
			return "", err
		}
		return t.diff(ctx, params)
	case "close":
		if err := params.allowOnly("operation", "repo"); err != nil {
			return "", err
		}
		return t.close(params)
	default:
		return "", fmt.Errorf("git: unsupported operation %q", params.Operation)
	}
}

func (t *GitTool) open(ctx context.Context, params gitArguments) (result string, err error) {
	address, err := requiredGitString(params.URL, "url")
	if err != nil {
		return "", err
	}
	address, err = validateGitURL(address)
	if err != nil {
		return "", err
	}

	ref := ""
	if params.Ref != nil {
		ref = strings.TrimSpace(*params.Ref)
		if err := validateGitRef(ref); err != nil {
			return "", err
		}
	}
	if err := t.reserveRepository(os.TempDir()); err != nil {
		return "", err
	}
	reserved := true
	defer func() {
		if reserved {
			t.releaseRepository()
		}
	}()

	root, err := os.MkdirTemp("", "kritui-git-")
	if err != nil {
		return "", fmt.Errorf("git: create temporary directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(root)
		}
	}()

	directory := filepath.Join(root, "repository")
	cloneArgs := []string{
		"clone",
		"--no-checkout",
		"--filter=blob:none",
		fmt.Sprintf("--depth=%d", gitCloneDepth),
		"--single-branch",
		"--no-tags",
	}
	if ref != "" {
		cloneArgs = append(cloneArgs, "--branch", ref)
	}
	cloneArgs = append(cloneArgs, "--", address, directory)
	if err := runGitCommand(ctx, gitCommandOptions{
		resourceRoot: root,
		timeout:      gitCloneTimeout,
	}, discardGitOutput, cloneArgs...); err != nil {
		return "", fmt.Errorf("git: clone repository: %w", err)
	}

	commitOutput, truncated, err := captureGitCommand(ctx, gitCommandOptions{
		directory:    directory,
		resourceRoot: root,
		timeout:      gitOperationTimeout,
	}, 128, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("git: resolve HEAD: %w", err)
	}
	if truncated {
		return "", errors.New("git: resolve HEAD: unexpected output")
	}
	commit := strings.TrimSpace(string(commitOutput))
	if !isHexCommit(commit, 40, 40) {
		return "", errors.New("git: resolve HEAD: invalid commit hash")
	}

	handle, err := t.newRepositoryHandle()
	if err != nil {
		return "", err
	}
	repository := &gitRepository{
		root:      root,
		directory: directory,
		commit:    commit,
	}
	refLabel := ref
	if refLabel == "" {
		refLabel = "remote HEAD"
	}
	result = fmt.Sprintf(
		"repo %s\ncommit %s\nref %s\nhistory depth %d\nexpires at request end or after 10m",
		handle,
		commit,
		refLabel,
		gitCloneDepth,
	)
	repository.returnedBytes = len(result)

	t.mu.Lock()
	if t.repositories == nil {
		t.repositories = make(map[string]*gitRepository)
	}
	t.repositories[handle] = repository
	t.mu.Unlock()
	reserved = false

	repository.timer = time.AfterFunc(gitRepositoryTTL, func() {
		_, _ = t.removeRepository(handle)
	})
	if done := ctx.Done(); done != nil {
		go func() {
			<-done
			_, _ = t.removeRepository(handle)
		}()
	}
	return result, nil
}

func (t *GitTool) tree(ctx context.Context, params gitArguments) (string, error) {
	handle, err := requiredGitHandle(params.Repo)
	if err != nil {
		return "", err
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 100, 500)
	if err != nil {
		return "", err
	}
	recursive := params.Recursive != nil && *params.Recursive

	return t.withRepository(handle, func(repository *gitRepository, maxOutput int) (string, error) {
		return gitTree(ctx, repository, repositoryPath, recursive, offset, limit, maxOutput)
	})
}

func (t *GitTool) search(ctx context.Context, params gitArguments) (string, error) {
	handle, err := requiredGitHandle(params.Repo)
	if err != nil {
		return "", err
	}
	query, err := requiredGitString(params.Query, "query")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(query) == "" {
		return "", errors.New("git: query must not be empty")
	}
	if len(query) > gitMaxQueryLength {
		return "", fmt.Errorf("git: query exceeds %d bytes", gitMaxQueryLength)
	}
	if hasUnsafeGitControl(query) {
		return "", errors.New("git: query must not contain control characters")
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 20, 100)
	if err != nil {
		return "", err
	}
	caseSensitive := params.CaseSensitive != nil && *params.CaseSensitive

	return t.withRepository(handle, func(repository *gitRepository, maxOutput int) (string, error) {
		return gitSearch(ctx, repository, query, repositoryPath, caseSensitive, offset, limit, maxOutput)
	})
}

func (t *GitTool) read(ctx context.Context, params gitArguments) (string, error) {
	handle, err := requiredGitHandle(params.Repo)
	if err != nil {
		return "", err
	}
	repositoryPath, err := requiredGitPath(params.Path)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 200, 400)
	if err != nil {
		return "", err
	}

	return t.withRepository(handle, func(repository *gitRepository, maxOutput int) (string, error) {
		return gitRead(ctx, repository, repositoryPath, offset, limit, maxOutput)
	})
}

func (t *GitTool) log(ctx context.Context, params gitArguments) (string, error) {
	handle, err := requiredGitHandle(params.Repo)
	if err != nil {
		return "", err
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 20, 50)
	if err != nil {
		return "", err
	}

	return t.withRepository(handle, func(repository *gitRepository, maxOutput int) (string, error) {
		return gitLog(ctx, repository, repositoryPath, offset, limit, maxOutput)
	})
}

func (t *GitTool) diff(ctx context.Context, params gitArguments) (string, error) {
	handle, err := requiredGitHandle(params.Repo)
	if err != nil {
		return "", err
	}
	base, err := requiredGitString(params.Base, "base")
	if err != nil {
		return "", err
	}
	base = strings.TrimSpace(base)
	if !isHexCommit(base, 7, 40) {
		return "", errors.New("git: base must be a 7 to 40 character hexadecimal commit hash")
	}
	repositoryPath, err := optionalGitPath(params.Path)
	if err != nil {
		return "", err
	}
	offset, limit, err := gitPage(params.Offset, params.Limit, 200, 400)
	if err != nil {
		return "", err
	}

	return t.withRepository(handle, func(repository *gitRepository, maxOutput int) (string, error) {
		return gitDiff(ctx, repository, base, repositoryPath, offset, limit, maxOutput)
	})
}

func (t *GitTool) close(params gitArguments) (string, error) {
	handle, err := requiredGitHandle(params.Repo)
	if err != nil {
		return "", err
	}
	found, err := t.removeRepository(handle)
	if err != nil {
		return "", fmt.Errorf("git: close repository: %w", err)
	}
	if !found {
		return "", errors.New("git: repository handle is unknown or expired")
	}
	return "closed " + handle, nil
}

func gitTree(ctx context.Context, repository *gitRepository, repositoryPath string, recursive bool, offset, limit, maxOutput int) (string, error) {
	treeish := "HEAD"
	if repositoryPath != "" {
		treeish += ":" + repositoryPath
	}
	args := []string{"ls-tree", "-z"}
	if recursive {
		args = append(args, "-r")
	}
	args = append(args, treeish)

	rootLabel := "."
	if repositoryPath != "" {
		rootLabel = repositoryPath + "/"
	}
	var output strings.Builder
	output.WriteString(rootLabel)
	output.WriteByte('\n')
	seenDirectories := make(map[string]struct{})
	skipped := 0
	displayed := 0
	truncated := false

	consume := func(reader io.Reader) (bool, error) {
		scanner := bufio.NewScanner(reader)
		scanner.Split(splitGitNUL)
		scanner.Buffer(make([]byte, 4096), gitMaxPathLength+256)
		for scanner.Scan() {
			entry, err := parseGitTreeEntry(scanner.Bytes())
			if err != nil {
				return false, err
			}
			if skipped < offset {
				skipped++
				continue
			}
			if displayed >= limit {
				truncated = true
				return true, nil
			}

			chunk, directories := formatGitTreeEntry(entry, recursive, seenDirectories)
			if output.Len()+len(chunk)+64 > maxOutput {
				truncated = true
				return true, nil
			}
			output.WriteString(chunk)
			for _, directory := range directories {
				seenDirectories[directory] = struct{}{}
			}
			displayed++
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("git: read tree: %w", err)
		}
		return false, nil
	}

	err := runGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
	}, consume, args...)
	if err != nil {
		return "", fmt.Errorf("git: list tree: %w", err)
	}
	if displayed == 0 {
		output.WriteString("(empty)\n")
	}
	if truncated {
		appendGitTruncation(&output, offset+displayed)
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func gitSearch(ctx context.Context, repository *gitRepository, query, repositoryPath string, caseSensitive bool, offset, limit, maxOutput int) (string, error) {
	args := []string{"grep", "-n", "-I", "-F", "-z", "--full-name"}
	if !caseSensitive {
		args = append(args, "-i")
	}
	args = append(args, "-e", query, "HEAD")
	if repositoryPath != "" {
		args = append(args, "--", literalGitPathspec(repositoryPath))
	}

	var output strings.Builder
	skipped := 0
	displayed := 0
	truncated := false
	consume := func(stream io.Reader) (bool, error) {
		reader := bufio.NewReaderSize(stream, 64*1024)
		for {
			match, found, err := readGitSearchMatch(reader)
			if err != nil {
				return false, err
			}
			if !found {
				return false, nil
			}
			if skipped < offset {
				skipped++
				continue
			}
			if displayed >= limit {
				truncated = true
				return true, nil
			}

			line := fmt.Sprintf("%s:%d:%s\n", printableGitPath(match.path), match.line, truncateGitText(match.text, 500))
			if output.Len()+len(line)+64 > maxOutput {
				truncated = true
				return true, nil
			}
			output.WriteString(line)
			displayed++
		}
	}

	err := runGitCommand(ctx, gitCommandOptions{
		directory:        repository.directory,
		resourceRoot:     repository.root,
		timeout:          gitOperationTimeout,
		allowExitCodeOne: true,
	}, consume, args...)
	if err != nil {
		return "", fmt.Errorf("git: search repository: %w", err)
	}
	if displayed == 0 {
		output.WriteString("(no matches)\n")
	}
	if truncated {
		appendGitTruncation(&output, offset+displayed)
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func gitRead(ctx context.Context, repository *gitRepository, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	var body strings.Builder
	lineNumber := 0
	displayed := 0
	truncated := false
	consume := func(reader io.Reader) (bool, error) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), gitMaxInputLineBytes)
		for scanner.Scan() {
			lineNumber++
			lineBytes := scanner.Bytes()
			if bytes.IndexByte(lineBytes, 0) >= 0 {
				return false, errors.New("git: file is binary")
			}
			if lineNumber <= offset {
				continue
			}
			if displayed >= limit {
				truncated = true
				return true, nil
			}

			line := truncateGitText(string(lineBytes), 2000)
			formatted := fmt.Sprintf("%6d\t%s\n", lineNumber, line)
			if body.Len()+len(formatted)+160 > maxOutput {
				truncated = true
				return true, nil
			}
			body.WriteString(formatted)
			displayed++
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("git: read file: source line exceeds %d bytes", gitMaxInputLineBytes)
		}
		return false, nil
	}

	err := runGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
	}, consume, "cat-file", "blob", "HEAD:"+repositoryPath)
	if err != nil {
		return "", fmt.Errorf("git: read %q: %w", repositoryPath, err)
	}

	var output strings.Builder
	if displayed == 0 {
		fmt.Fprintf(&output, "%s (no lines at offset %d)", printableGitPath(repositoryPath), offset)
		return output.String(), nil
	}
	fmt.Fprintf(&output, "%s (lines %d-%d)\n", printableGitPath(repositoryPath), offset+1, offset+displayed)
	output.WriteString(body.String())
	if truncated {
		appendGitTruncation(&output, offset+displayed)
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func gitLog(ctx context.Context, repository *gitRepository, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	args := []string{
		"log",
		"-z",
		"--no-decorate",
		"--format=%H%x00%aI%x00%an%x00%s",
		fmt.Sprintf("--skip=%d", offset),
		fmt.Sprintf("--max-count=%d", limit+1),
		"HEAD",
	}
	if repositoryPath != "" {
		args = append(args, "--", literalGitPathspec(repositoryPath))
	}

	var output strings.Builder
	displayed := 0
	truncated := false
	consume := func(stream io.Reader) (bool, error) {
		reader := bufio.NewReaderSize(stream, 64*1024)
		for {
			fields := make([]string, 4)
			for index := range fields {
				field, terminated, err := readGitDelimited(reader, 0, gitMaxInputLineBytes)
				if err != nil {
					return false, fmt.Errorf("git: read log: %w", err)
				}
				if !terminated {
					if index == 0 && len(field) == 0 {
						return false, nil
					}
					return false, errors.New("git: read log: malformed output")
				}
				fields[index] = string(field)
			}
			if displayed >= limit {
				truncated = true
				return true, nil
			}

			hash := fields[0]
			if len(hash) > 12 {
				hash = hash[:12]
			}
			line := fmt.Sprintf(
				"%s  %s  %s  %s\n",
				hash,
				truncateGitText(fields[1], 40),
				truncateGitText(fields[2], 100),
				truncateGitText(fields[3], 500),
			)
			if output.Len()+len(line)+64 > maxOutput {
				truncated = true
				return true, nil
			}
			output.WriteString(line)
			displayed++
		}
	}

	err := runGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
	}, consume, args...)
	if err != nil {
		return "", fmt.Errorf("git: read log: %w", err)
	}
	if displayed == 0 {
		output.WriteString("(no commits at offset)\n")
	}
	if truncated {
		appendGitTruncation(&output, offset+displayed)
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func gitDiff(ctx context.Context, repository *gitRepository, base, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	_, truncated, err := captureGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
	}, 1, "cat-file", "-e", base+"^{commit}")
	if err != nil {
		return "", errors.New("git: base commit is unavailable in shallow history")
	}
	if truncated {
		return "", errors.New("git: verify base commit: unexpected output")
	}

	args := []string{
		"diff",
		"--no-ext-diff",
		"--no-textconv",
		"--no-renames",
		"--unified=3",
		base,
		"HEAD",
	}
	if repositoryPath != "" {
		args = append(args, "--", literalGitPathspec(repositoryPath))
	}
	return pageGitLines(ctx, repository, args, offset, limit, maxOutput, "(no differences)")
}

func pageGitLines(ctx context.Context, repository *gitRepository, args []string, offset, limit, maxOutput int, empty string) (string, error) {
	var output strings.Builder
	skipped := 0
	displayed := 0
	truncated := false
	consume := func(reader io.Reader) (bool, error) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), gitMaxInputLineBytes)
		for scanner.Scan() {
			if skipped < offset {
				skipped++
				continue
			}
			if displayed >= limit {
				truncated = true
				return true, nil
			}
			line := truncateGitText(string(scanner.Bytes()), 4000) + "\n"
			if output.Len()+len(line)+64 > maxOutput {
				truncated = true
				return true, nil
			}
			output.WriteString(line)
			displayed++
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("git: command output line exceeds %d bytes", gitMaxInputLineBytes)
		}
		return false, nil
	}

	err := runGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
	}, consume, args...)
	if err != nil {
		return "", fmt.Errorf("git: read diff: %w", err)
	}
	if displayed == 0 {
		output.WriteString(empty)
		output.WriteByte('\n')
	}
	if truncated {
		appendGitTruncation(&output, offset+displayed)
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func (t *GitTool) withRepository(handle string, operation func(*gitRepository, int) (string, error)) (string, error) {
	t.mu.Lock()
	repository := t.repositories[handle]
	t.mu.Unlock()
	if repository == nil {
		return "", errors.New("git: repository handle is unknown or expired; call open again")
	}

	repository.mu.Lock()
	if repository.closed {
		repository.mu.Unlock()
		return "", errors.New("git: repository handle is unknown or expired; call open again")
	}
	remaining := gitMaxRepositoryOutput - repository.returnedBytes
	if remaining < 4096 {
		repository.mu.Unlock()
		return "", errors.New("git: repository output budget exhausted; answer from current results or close the repository")
	}
	maxOutput := min(gitMaxResultBytes, remaining)
	result, err := operation(repository, maxOutput)
	if err == nil && len(result) > maxOutput {
		err = errors.New("git: result exceeded output limit")
		result = ""
	}
	if err == nil {
		repository.returnedBytes += len(result)
	}
	resourceFailure := errors.Is(err, errGitResourceLimit)
	repository.mu.Unlock()

	if resourceFailure {
		_, _ = t.removeRepository(handle)
	}
	return result, err
}

func (t *GitTool) reserveRepository(directory string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.active >= gitMaxRepositories {
		return fmt.Errorf("git: at most %d repositories may be open", gitMaxRepositories)
	}
	available, err := availableDiskBytes(directory)
	if err != nil {
		return fmt.Errorf("git: check free disk space: %w", err)
	}
	required := uint64(gitFreeSpaceReserve) + uint64(t.active+1)*uint64(gitRepositoryMaxSize)
	if available < required {
		return fmt.Errorf(
			"git: insufficient free disk space for %d repository reservations: need at least %s, have %s",
			t.active+1,
			formatGitBytes(required),
			formatGitBytes(available),
		)
	}
	t.active++
	return nil
}

func (t *GitTool) releaseRepository() {
	t.mu.Lock()
	if t.active > 0 {
		t.active--
	}
	t.mu.Unlock()
}

func (t *GitTool) newRepositoryHandle() (string, error) {
	for range 4 {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("git: create repository handle: %w", err)
		}
		handle := hex.EncodeToString(random)
		t.mu.Lock()
		_, exists := t.repositories[handle]
		t.mu.Unlock()
		if !exists {
			return handle, nil
		}
	}
	return "", errors.New("git: could not create unique repository handle")
}

func (t *GitTool) removeRepository(handle string) (bool, error) {
	t.mu.Lock()
	repository := t.repositories[handle]
	if repository != nil {
		delete(t.repositories, handle)
		if t.active > 0 {
			t.active--
		}
	}
	t.mu.Unlock()
	if repository == nil {
		return false, nil
	}

	repository.mu.Lock()
	defer repository.mu.Unlock()
	if repository.closed {
		return true, nil
	}
	repository.closed = true
	if repository.timer != nil {
		repository.timer.Stop()
	}
	return true, os.RemoveAll(repository.root)
}

func parseGitArguments(arguments json.RawMessage) (gitArguments, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &values); err != nil || values == nil {
		return gitArguments{}, errors.New("git: arguments must be a JSON object")
	}
	known := map[string]struct{}{
		"operation":      {},
		"url":            {},
		"ref":            {},
		"repo":           {},
		"path":           {},
		"query":          {},
		"case_sensitive": {},
		"recursive":      {},
		"base":           {},
		"offset":         {},
		"limit":          {},
	}
	provided := make(map[string]struct{}, len(values))
	for name, value := range values {
		if _, ok := known[name]; !ok {
			return gitArguments{}, fmt.Errorf("git: unknown argument %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return gitArguments{}, fmt.Errorf("git: argument %q must not be null", name)
		}
		provided[name] = struct{}{}
	}

	var params gitArguments
	if err := json.Unmarshal(arguments, &params); err != nil {
		return gitArguments{}, fmt.Errorf("git: invalid arguments: %w", err)
	}
	params.Operation = strings.TrimSpace(params.Operation)
	if params.Operation == "" {
		return gitArguments{}, errors.New("git: operation must be a string")
	}
	params.provided = provided
	return params, nil
}

func (params gitArguments) allowOnly(names ...string) error {
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowed[name] = struct{}{}
	}
	for name := range params.provided {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("git: argument %q is not valid for operation %q", name, params.Operation)
		}
	}
	return nil
}

func requiredGitString(value *string, name string) (string, error) {
	if value == nil {
		return "", fmt.Errorf("git: %s is required", name)
	}
	return *value, nil
}

func requiredGitHandle(value *string) (string, error) {
	handle, err := requiredGitString(value, "repo")
	if err != nil {
		return "", err
	}
	if !isHexCommit(handle, 32, 32) {
		return "", errors.New("git: repo must be a handle returned by open")
	}
	return strings.ToLower(handle), nil
}

func validateGitURL(address string) (string, error) {
	address = strings.TrimSpace(address)
	if len(address) > 2048 {
		return "", errors.New("git: repository URL exceeds 2048 bytes")
	}
	if hasUnsafeGitControl(address) {
		return "", errors.New("git: repository URL must not contain control characters")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", errors.New("git: repository URL must be a fully formed HTTPS URL")
	}
	if parsed.User != nil {
		return "", errors.New("git: repository URL must not contain credentials")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", errors.New("git: repository URL must use the default HTTPS port")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("git: repository URL must not contain a query or fragment")
	}
	if _, allowed := gitAllowedHosts[strings.ToLower(parsed.Hostname())]; !allowed {
		return "", errors.New("git: repository host must be github.com, gitlab.com, or bitbucket.org")
	}
	if strings.Trim(parsed.Path, "/") == "" {
		return "", errors.New("git: repository URL must contain a repository path")
	}
	return parsed.String(), nil
}

func validateGitRef(ref string) error {
	if ref == "" {
		return errors.New("git: ref must not be empty")
	}
	if len(ref) > gitMaxRefLength {
		return fmt.Errorf("git: ref exceeds %d bytes", gitMaxRefLength)
	}
	if strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, ".") || strings.HasSuffix(ref, ".") ||
		strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".lock") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") {
		return errors.New("git: ref is not a valid branch or tag")
	}
	for _, character := range ref {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || strings.ContainsRune("-._/", character) {
			continue
		}
		return errors.New("git: ref is not a valid branch or tag")
	}
	return nil
}

func requiredGitPath(value *string) (string, error) {
	if value == nil {
		return "", errors.New("git: path is required")
	}
	cleaned, err := validateGitPath(*value)
	if err != nil {
		return "", err
	}
	if cleaned == "" {
		return "", errors.New("git: path must identify a file")
	}
	return cleaned, nil
}

func optionalGitPath(value *string) (string, error) {
	if value == nil || *value == "" {
		return "", nil
	}
	return validateGitPath(*value)
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
	cleaned := path.Clean(repositoryPath)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("git: path must not traverse outside the repository")
	}
	return cleaned, nil
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
		return 0, 0, fmt.Errorf("git: limit must be between 1 and %d for this operation", maxLimit)
	}
	return offset, limit, nil
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
		return character == 0 || character == '\n' || character == '\r' || unicode.IsControl(character)
	}) >= 0
}

func literalGitPathspec(repositoryPath string) string {
	return ":(literal)" + repositoryPath
}

func runGitCommand(ctx context.Context, options gitCommandOptions, consume func(io.Reader) (bool, error), arguments ...string) error {
	if err := checkGitResources(options.resourceRoot); err != nil {
		return err
	}
	commandContext, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()

	commandArguments := append(gitCommandPrefix(), arguments...)
	command := exec.CommandContext(commandContext, "git", commandArguments...)
	command.Dir = options.directory
	command.Env = gitEnvironment()
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		return killGitCommand(command)
	}
	command.WaitDelay = 2 * time.Second

	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open stdout: %w", err)
	}
	var stderr limitedGitBuffer
	stderr.limit = gitMaxErrorBytes
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start git: %w", err)
	}

	monitorContext, stopMonitor := context.WithCancel(context.Background())
	monitorResult := make(chan error, 1)
	go func() {
		monitorResult <- monitorGitResources(monitorContext, command, options.resourceRoot)
	}()

	stopped, consumeErr := consume(stdout)
	if consumeErr != nil || stopped {
		_ = killGitCommand(command)
	}
	waitErr := command.Wait()
	stopMonitor()
	monitorErr := <-monitorResult
	postCheckErr := checkGitResources(options.resourceRoot)

	if monitorErr != nil {
		return monitorErr
	}
	if postCheckErr != nil {
		return postCheckErr
	}
	if consumeErr != nil {
		return consumeErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return errors.New("git command timed out")
	}
	if stopped {
		return nil
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if options.allowExitCodeOne && errors.As(waitErr, &exitError) && exitError.ExitCode() == 1 {
			return nil
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			return fmt.Errorf("git command failed: %w", waitErr)
		}
		return fmt.Errorf("git command failed: %s", message)
	}
	return nil
}

func gitCommandPrefix() []string {
	return []string{
		"-c", "credential.helper=",
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=never",
		"-c", "http.followRedirects=false",
		"-c", "color.ui=false",
		"-c", "core.pager=cat",
	}
}

func gitEnvironment() []string {
	environment := make([]string, 0, len(os.Environ())+8)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(name, "GIT_") {
			continue
		}
		environment = append(environment, variable)
	}
	return append(environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"LC_ALL=C",
	)
}

func monitorGitResources(ctx context.Context, command *exec.Cmd, root string) error {
	ticker := time.NewTicker(gitResourceCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := checkGitResources(root); err != nil {
				_ = killGitCommand(command)
				return err
			}
		}
	}
}

func killGitCommand(command *exec.Cmd) error {
	if command.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func checkGitResources(root string) error {
	if root == "" {
		return nil
	}
	size, err := gitDirectorySize(root)
	if err != nil {
		return fmt.Errorf("%w: measure repository: %v", errGitResourceLimit, err)
	}
	if size > gitRepositoryMaxSize {
		return fmt.Errorf("%w: repository exceeds %s", errGitResourceLimit, formatGitBytes(uint64(gitRepositoryMaxSize)))
	}
	available, err := availableDiskBytes(root)
	if err != nil {
		return fmt.Errorf("%w: check free disk space: %v", errGitResourceLimit, err)
	}
	if available < uint64(gitFreeSpaceReserve) {
		return fmt.Errorf("%w: free disk space fell below %s", errGitResourceLimit, formatGitBytes(uint64(gitFreeSpaceReserve)))
	}
	return nil
}

func gitDirectorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if errors.Is(walkErr, os.ErrNotExist) {
				return nil
			}
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		total += info.Size()
		if total > gitRepositoryMaxSize {
			return errGitStopWalk
		}
		return nil
	})
	if errors.Is(err, errGitStopWalk) {
		return total, nil
	}
	return total, err
}

func availableDiskBytes(directory string) (uint64, error) {
	var statistics syscall.Statfs_t
	if err := syscall.Statfs(directory, &statistics); err != nil {
		return 0, err
	}
	blocks := uint64(statistics.Bavail)
	blockSize := uint64(statistics.Bsize)
	if blockSize != 0 && blocks > ^uint64(0)/blockSize {
		return ^uint64(0), nil
	}
	return blocks * blockSize, nil
}

func formatGitBytes(size uint64) string {
	const mebibyte = 1024 * 1024
	return fmt.Sprintf("%d MiB", size/mebibyte)
}

func captureGitCommand(ctx context.Context, options gitCommandOptions, limit int, arguments ...string) ([]byte, bool, error) {
	var output []byte
	truncated := false
	consume := func(reader io.Reader) (bool, error) {
		content, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
		if err != nil {
			return false, err
		}
		if len(content) > limit {
			content = content[:limit]
			truncated = true
		}
		output = content
		return truncated, nil
	}
	err := runGitCommand(ctx, options, consume, arguments...)
	return output, truncated, err
}

func discardGitOutput(reader io.Reader) (bool, error) {
	_, err := io.Copy(io.Discard, reader)
	return false, err
}

type limitedGitBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (buffer *limitedGitBuffer) Write(content []byte) (int, error) {
	written := len(content)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if len(content) > remaining {
			content = content[:remaining]
		}
		_, _ = buffer.buffer.Write(content)
	}
	return written, nil
}

func (buffer *limitedGitBuffer) String() string {
	return buffer.buffer.String()
}

func splitGitNUL(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if index := bytes.IndexByte(data, 0); index >= 0 {
		return index + 1, data[:index], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func parseGitTreeEntry(record []byte) (gitTreeEntry, error) {
	metadata, name, found := bytes.Cut(record, []byte{'\t'})
	if !found {
		return gitTreeEntry{}, errors.New("git: malformed tree output")
	}
	fields := bytes.Fields(metadata)
	if len(fields) != 3 {
		return gitTreeEntry{}, errors.New("git: malformed tree metadata")
	}
	return gitTreeEntry{
		name:       strings.ToValidUTF8(string(name), "\uFFFD"),
		objectType: string(fields[1]),
	}, nil
}

func formatGitTreeEntry(entry gitTreeEntry, recursive bool, seenDirectories map[string]struct{}) (string, []string) {
	if !recursive {
		name := printableGitTreeName(entry.name)
		if entry.objectType == "tree" {
			name += "/"
		} else if entry.objectType == "commit" {
			name += " [submodule]"
		}
		return "|-- " + name + "\n", nil
	}

	components := strings.Split(entry.name, "/")
	var output strings.Builder
	directories := make([]string, 0, len(components)-1)
	for index := 0; index < len(components)-1; index++ {
		directory := strings.Join(components[:index+1], "/")
		if _, seen := seenDirectories[directory]; seen {
			continue
		}
		output.WriteString(strings.Repeat("|   ", index))
		output.WriteString("|-- ")
		output.WriteString(printableGitTreeName(components[index]))
		output.WriteString("/\n")
		directories = append(directories, directory)
	}
	output.WriteString(strings.Repeat("|   ", len(components)-1))
	output.WriteString("|-- ")
	output.WriteString(printableGitTreeName(components[len(components)-1]))
	if entry.objectType == "commit" {
		output.WriteString(" [submodule]")
	}
	output.WriteByte('\n')
	return output.String(), directories
}

func readGitSearchMatch(reader *bufio.Reader) (gitSearchMatch, bool, error) {
	filename, terminated, err := readGitDelimited(reader, 0, gitMaxPathLength+16)
	if err != nil {
		return gitSearchMatch{}, false, fmt.Errorf("git: read search path: %w", err)
	}
	if !terminated {
		if len(filename) == 0 {
			return gitSearchMatch{}, false, nil
		}
		return gitSearchMatch{}, false, errors.New("git: malformed search output")
	}
	lineValue, terminated, err := readGitDelimited(reader, 0, 32)
	if err != nil || !terminated {
		return gitSearchMatch{}, false, errors.New("git: malformed search line number")
	}
	text, _, err := readGitDelimited(reader, '\n', gitMaxInputLineBytes)
	if err != nil {
		return gitSearchMatch{}, false, fmt.Errorf("git: read search result: %w", err)
	}
	line, err := strconv.Atoi(string(lineValue))
	if err != nil || line < 1 {
		return gitSearchMatch{}, false, errors.New("git: malformed search line number")
	}
	name := strings.TrimPrefix(string(filename), "HEAD:")
	return gitSearchMatch{
		path: strings.ToValidUTF8(name, "\uFFFD"),
		line: line,
		text: strings.ToValidUTF8(string(text), "\uFFFD"),
	}, true, nil
}

func readGitDelimited(reader *bufio.Reader, delimiter byte, maximum int) ([]byte, bool, error) {
	var result []byte
	for {
		fragment, err := reader.ReadSlice(delimiter)
		terminated := len(fragment) > 0 && fragment[len(fragment)-1] == delimiter
		if terminated {
			fragment = fragment[:len(fragment)-1]
		}
		if len(result)+len(fragment) > maximum {
			return nil, false, fmt.Errorf("field exceeds %d bytes", maximum)
		}
		result = append(result, fragment...)
		if terminated {
			return result, true, nil
		}
		if err == nil || errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return result, false, nil
		}
		return nil, false, err
	}
}

func appendGitTruncation(output *strings.Builder, nextOffset int) {
	if output.Len() > 0 && !strings.HasSuffix(output.String(), "\n") {
		output.WriteByte('\n')
	}
	fmt.Fprintf(output, "[truncated; next_offset=%d]\n", nextOffset)
}

func printableGitPath(value string) string {
	if strings.ContainsAny(value, ":\t\n\r") || hasUnsafeGitControl(value) {
		return strconv.QuoteToASCII(value)
	}
	return value
}

func printableGitTreeName(value string) string {
	if value == "" || strings.IndexFunc(value, func(character rune) bool {
		return unicode.IsControl(character) || !unicode.IsPrint(character)
	}) >= 0 {
		return strconv.QuoteToASCII(value)
	}
	return value
}

func truncateGitText(value string, maximumRunes int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.Map(func(character rune) rune {
		if character == '\t' || (unicode.IsPrint(character) && !unicode.IsControl(character)) {
			return character
		}
		return '\uFFFD'
	}, value)
	if utf8.RuneCountInString(value) <= maximumRunes {
		return value
	}
	end := 0
	count := 0
	for index := range value {
		if count == maximumRunes {
			end = index
			break
		}
		count++
	}
	return value[:end] + "... [truncated]"
}
