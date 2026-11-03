package git

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	gitTreeTruncation = "[truncated; use a narrower path, lower depth, or larger limit]"
	gitTreeEmpty      = "(empty)"
)

type gitTreeEntry struct {
	name       string
	objectType string
}

type gitTreeNode struct {
	name     string
	isDir    bool
	expanded bool
	children []*gitTreeNode
}

type gitSearchMatch struct {
	path string
	line int
	text string
}

type gitBranchEntry struct {
	name          string
	hash          string
	date          string
	subject       string
	timestamp     int64
	defaultBranch bool
}

func gitTree(ctx context.Context, repository *gitRepository, ref, revision, repositoryPath string, depth, limit, maxOutput int) (string, error) {
	commit, err := resolveGitCommit(ctx, repository, ref, revision)
	if err != nil {
		return "", err
	}
	if repositoryPath != "" {
		objectType, err := gitObjectType(ctx, repository, commit+":"+repositoryPath)
		if err != nil {
			return "", fmt.Errorf("git: path %q is not available at %s", printableGitPath(repositoryPath), shortGitCommit(commit))
		}
		if objectType != "tree" {
			return "", fmt.Errorf("git: path %q is a file; use git_read", printableGitPath(repositoryPath))
		}
	}

	remaining := limit
	truncated := false
	children, more, err := buildGitTree(ctx, repository, commit, repositoryPath, 1, depth, &remaining)
	if err != nil {
		return "", err
	}
	truncated = more
	rootLabel := "."
	if repositoryPath != "" {
		rootLabel = printableGitPath(repositoryPath) + "/"
	}
	rootLabel += " @ " + shortGitCommit(commit)
	result, formattedTruncated := formatGitTree(rootLabel, children, maxOutput)
	if formattedTruncated {
		truncated = true
	}
	if truncated {
		result += "\n" + gitTreeTruncation
	}
	if len(result) > maxOutput {
		return "", errors.New("git: result exceeded output limit")
	}
	return result, nil
}

func buildGitTree(ctx context.Context, repository *gitRepository, commit, treePath string, level, maxDepth int, remaining *int) ([]*gitTreeNode, bool, error) {
	entries, listMore, err := listGitTreeEntries(ctx, repository, commit, treePath, *remaining+1)
	if err != nil {
		return nil, false, err
	}
	nodes := make([]*gitTreeNode, 0, min(len(entries), *remaining))
	truncated := listMore
	for _, entry := range entries {
		if *remaining <= 0 {
			truncated = true
			break
		}
		*remaining--
		node := &gitTreeNode{
			name:  entry.name,
			isDir: entry.objectType == "tree",
		}
		if node.isDir && level < maxDepth && *remaining > 0 {
			childPath := joinGitTreePath(treePath, entry.name)
			children, childMore, err := buildGitTree(ctx, repository, commit, childPath, level+1, maxDepth, remaining)
			if err != nil {
				return nil, false, err
			}
			node.expanded = true
			node.children = children
			if childMore {
				truncated = true
			}
		} else if node.isDir && level < maxDepth {
			truncated = true
		}
		nodes = append(nodes, node)
	}
	return nodes, truncated, nil
}

func listGitTreeEntries(ctx context.Context, repository *gitRepository, commit, treePath string, maximum int) ([]gitTreeEntry, bool, error) {
	treeish := commit
	if treePath != "" {
		treeish += ":" + treePath
	}
	entries := make([]gitTreeEntry, 0, min(maximum, 64))
	more := false
	consume := func(reader io.Reader) (bool, error) {
		scanner := bufio.NewScanner(reader)
		scanner.Split(splitGitNUL)
		scanner.Buffer(make([]byte, 4096), gitMaxPathLength+256)
		for scanner.Scan() {
			if len(entries) >= maximum {
				more = true
				return true, nil
			}
			entry, err := parseGitTreeEntry(scanner.Bytes())
			if err != nil {
				return false, err
			}
			entries = append(entries, entry)
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("git: read tree: %w", err)
		}
		return false, nil
	}
	if err := runGitCommand(ctx, gitCommandOptions{
		directory: repository.directory,
		timeout:   gitOperationTimeout,
		secrets:   repository.secrets,
	}, consume, "ls-tree", "-z", treeish); err != nil {
		return nil, false, fmt.Errorf("git: list tree: %w", err)
	}
	return entries, more, nil
}

func formatGitTree(rootLabel string, children []*gitTreeNode, maxOutput int) (string, bool) {
	var output strings.Builder
	output.WriteString(rootLabel)
	output.WriteByte('\n')
	budget := maxOutput - len(gitTreeTruncation) - 1
	truncated := false
	if len(children) == 0 {
		output.WriteString(gitTreeEmpty)
		return output.String(), false
	}
	writeGitTreeNodes(&output, children, "", budget, &truncated)
	result := strings.TrimRight(output.String(), "\n")
	return result, truncated
}

func writeGitTreeNodes(output *strings.Builder, nodes []*gitTreeNode, prefix string, budget int, truncated *bool) {
	for index, node := range nodes {
		if output.Len() >= budget {
			*truncated = true
			return
		}
		last := index == len(nodes)-1
		connector := "├── "
		childPrefix := prefix + "│   "
		if last {
			connector = "└── "
			childPrefix = prefix + "    "
		}
		line := prefix + connector + printableGitTreeName(node.name)
		if node.isDir {
			line += "/"
		}
		if output.Len()+len(line)+1 > budget {
			*truncated = true
			return
		}
		output.WriteString(line)
		output.WriteByte('\n')
		if !node.isDir {
			continue
		}
		if node.expanded && len(node.children) == 0 {
			emptyLine := childPrefix + gitTreeEmpty
			if output.Len()+len(emptyLine)+1 > budget {
				*truncated = true
				return
			}
			output.WriteString(emptyLine)
			output.WriteByte('\n')
			continue
		}
		writeGitTreeNodes(output, node.children, childPrefix, budget, truncated)
	}
}

func joinGitTreePath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func gitRead(ctx context.Context, repository *gitRepository, ref, revision, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	commit, err := resolveGitCommit(ctx, repository, ref, revision)
	if err != nil {
		return "", err
	}
	objectType, err := gitObjectType(ctx, repository, commit+":"+repositoryPath)
	if err != nil {
		return "", fmt.Errorf("git: path %q is not available at %s", printableGitPath(repositoryPath), shortGitCommit(commit))
	}
	if objectType == "tree" {
		return "", fmt.Errorf("git: path %q is a directory; use git_tree", printableGitPath(repositoryPath))
	}
	if objectType != "blob" {
		return "", fmt.Errorf("git: path %q is not a file", printableGitPath(repositoryPath))
	}

	header := fmt.Sprintf("%s @ %s", printableGitPath(repositoryPath), shortGitCommit(commit))
	var body strings.Builder
	lineNumber := 0
	displayed := 0
	truncated := false
	consume := func(reader io.Reader) (bool, error) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), gitMaxInputLine)
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
			if len(header)+1+body.Len()+len(formatted)+64 > maxOutput {
				truncated = true
				return true, nil
			}
			body.WriteString(formatted)
			displayed++
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("git: source line exceeds %d bytes", gitMaxInputLine)
		}
		return false, nil
	}
	if err := runGitCommand(ctx, gitCommandOptions{
		directory: repository.directory,
		timeout:   gitOperationTimeout,
		secrets:   repository.secrets,
	}, consume, "cat-file", "blob", commit+":"+repositoryPath); err != nil {
		return "", fmt.Errorf("git: read %q: %w", printableGitPath(repositoryPath), err)
	}

	var output strings.Builder
	output.WriteString(header)
	output.WriteByte('\n')
	if displayed == 0 {
		if offset == 0 {
			output.WriteString("(empty file)")
		} else {
			fmt.Fprintf(&output, "(no lines at offset %d)", offset)
		}
		return output.String(), nil
	}
	output.WriteString(body.String())
	if truncated {
		appendGitTruncation(&output, offset+displayed)
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func gitSearch(ctx context.Context, repository *gitRepository, ref, revision, query, repositoryPath string, regex, caseSensitive bool, offset, limit, maxOutput int) (string, error) {
	commit, err := resolveGitCommit(ctx, repository, ref, revision)
	if err != nil {
		return "", err
	}
	args := []string{"grep", "-n", "-I", "-z", "--full-name"}
	if regex {
		args = append(args, "-E")
	} else {
		args = append(args, "-F")
	}
	if !caseSensitive {
		args = append(args, "-i")
	}
	args = append(args, "-e", query, commit)
	if repositoryPath != "" {
		args = append(args, "--", literalGitPathspec(repositoryPath))
	}

	header := ". @ " + shortGitCommit(commit)
	if repositoryPath != "" {
		header = printableGitPath(repositoryPath) + " @ " + shortGitCommit(commit)
	}
	var output strings.Builder
	output.WriteString(header)
	output.WriteByte('\n')
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
			match.path = strings.TrimPrefix(match.path, commit+":")
			line := fmt.Sprintf("%s:%d:%s\n", printableGitPath(match.path), match.line, truncateGitText(match.text, 500))
			if output.Len()+len(line)+64 > maxOutput {
				truncated = true
				return true, nil
			}
			output.WriteString(line)
			displayed++
		}
	}
	if err := runGitCommand(ctx, gitCommandOptions{
		directory:        repository.directory,
		timeout:          gitOperationTimeout,
		allowExitCodeOne: true,
		secrets:          repository.secrets,
	}, consume, args...); err != nil {
		return "", fmt.Errorf("git: search repository: %w", err)
	}
	if displayed == 0 {
		if offset == 0 {
			output.WriteString("(no matches)\n")
		} else {
			fmt.Fprintf(&output, "(no matches at offset %d)\n", offset)
		}
	}
	if truncated {
		appendGitTruncation(&output, offset+displayed)
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func gitLog(ctx context.Context, repository *gitRepository, ref, revision, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	commit, err := resolveGitCommit(ctx, repository, ref, revision)
	if err != nil {
		return "", err
	}
	args := []string{
		"log",
		"-z",
		"--no-decorate",
		"--format=%H%x00%as%x00%an%x00%s",
		fmt.Sprintf("--skip=%d", offset),
		fmt.Sprintf("--max-count=%d", limit+1),
		commit,
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
				field, terminated, err := readGitDelimited(reader, 0, gitMaxInputLine)
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
			line := fmt.Sprintf("%s  %s  %s  %s\n",
				shortGitCommit(fields[0]),
				truncateGitField(fields[1], 40),
				truncateGitField(fields[2], 80),
				truncateGitField(fields[3], 300),
			)
			if output.Len()+len(line)+64 > maxOutput {
				truncated = true
				return true, nil
			}
			output.WriteString(line)
			displayed++
		}
	}
	if err := runGitCommand(ctx, gitCommandOptions{
		directory: repository.directory,
		timeout:   gitOperationTimeout,
		secrets:   repository.secrets,
	}, consume, args...); err != nil {
		return "", fmt.Errorf("git: read log: %w", err)
	}
	if displayed == 0 {
		if offset == 0 {
			output.WriteString("(no commits)\n")
		} else {
			fmt.Fprintf(&output, "(no commits at offset %d)\n", offset)
		}
	}
	if truncated {
		appendGitTruncation(&output, offset+displayed)
	}
	return strings.TrimRight(output.String(), "\n"), nil
}

func gitDiff(ctx context.Context, repository *gitRepository, ref, base, target, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	baseCommit, err := resolveGitCommitValue(ctx, repository, base)
	if err != nil {
		return "", fmt.Errorf("git: resolve base: %w", err)
	}
	if target == "" {
		target = ref
	}
	if target == "" {
		target = "HEAD"
	}
	targetCommit, err := resolveGitCommitValue(ctx, repository, target)
	if err != nil {
		return "", fmt.Errorf("git: resolve target: %w", err)
	}
	args := []string{
		"diff",
		"--no-ext-diff",
		"--no-textconv",
		"--no-renames",
		"--unified=3",
		baseCommit,
		targetCommit,
	}
	if repositoryPath != "" {
		args = append(args, "--", literalGitPathspec(repositoryPath))
	}
	empty := "(no differences)"
	if offset > 0 {
		empty = fmt.Sprintf("(no diff lines at offset %d)", offset)
	}
	return pageGitLines(ctx, repository, args, offset, limit, maxOutput, empty)
}

func gitBranches(ctx context.Context, repository *gitRepository, includeTags bool, offset, limit, maxOutput int) (string, error) {
	branches, err := listGitBranches(ctx, repository)
	if err != nil {
		return "", err
	}
	defaultBranch := gitDefaultBranch(ctx, repository)
	for index := range branches {
		if defaultBranch != "" && branches[index].name == defaultBranch {
			branches[index].defaultBranch = true
		}
		branches[index].name = printableGitPath(branches[index].name)
	}
	var tags []gitBranchEntry
	if includeTags {
		tags, err = listGitTags(ctx, repository)
		if err != nil {
			return "", err
		}
		for index := range tags {
			tags[index].name = printableGitPath(tags[index].name)
		}
	}

	total := len(branches)
	if includeTags {
		total += len(tags)
	}
	if total == 0 {
		var output strings.Builder
		output.WriteString("(no branches)")
		if includeTags {
			output.WriteString("\ntags:\n" + gitTreeEmpty)
		}
		return output.String(), nil
	}
	if offset >= total {
		return fmt.Sprintf("(no entries at offset %d)", offset), nil
	}

	var output strings.Builder
	budget := maxOutput - len("[truncated; next_offset=9999999999]") - 1
	truncated := false
	nextOffset := offset
	shown := 0
	if offset < len(branches) {
		for index := offset; index < len(branches) && shown < limit; index++ {
			line := formatGitBranchLine(branches[index])
			if output.Len()+len(line)+1 > budget {
				truncated = true
				nextOffset = index
				break
			}
			output.WriteString(line)
			output.WriteByte('\n')
			shown++
			nextOffset = index + 1
		}
	}
	if !truncated && includeTags && shown < limit {
		tagStart := 0
		if offset > len(branches) {
			tagStart = offset - len(branches)
		}
		if shown > 0 || offset >= len(branches) {
			if len(tags) == 0 && offset <= len(branches) {
				line := "tags:\n"
				if output.Len()+len(line)+len(gitTreeEmpty)+1 <= budget {
					output.WriteString(line)
					output.WriteString(gitTreeEmpty)
					output.WriteByte('\n')
				}
			} else if tagStart < len(tags) {
				if output.Len()+len("tags:\n")+1 > budget {
					truncated = true
					nextOffset = len(branches) + tagStart
				} else {
					output.WriteString("tags:\n")
					for index := tagStart; index < len(tags) && shown < limit; index++ {
						line := formatGitBranchLine(tags[index])
						if output.Len()+len(line)+1 > budget {
							truncated = true
							nextOffset = len(branches) + index
							break
						}
						output.WriteString(line)
						output.WriteByte('\n')
						shown++
						nextOffset = len(branches) + index + 1
					}
				}
			}
		}
	}
	if !truncated && offset+shown < total {
		truncated = true
		nextOffset = offset + shown
	}
	result := strings.TrimRight(output.String(), "\n")
	if truncated {
		result += fmt.Sprintf("\n[truncated; next_offset=%d]", nextOffset)
	}
	if len(result) > maxOutput {
		return "", errors.New("git: result exceeded output limit")
	}
	return result, nil
}

func listGitBranches(ctx context.Context, repository *gitRepository) ([]gitBranchEntry, error) {
	records, err := listForEachRef(ctx, repository, "refs/heads",
		"%(refname:strip=2)%00%(objectname)%00%(committerdate:unix)%00%(committerdate:short)%00%(subject)")
	if err != nil {
		return nil, err
	}
	branches := make([]gitBranchEntry, 0, len(records))
	for _, fields := range records {
		if len(fields) != 5 {
			return nil, errors.New("git: malformed branch output")
		}
		timestamp, _ := strconv.ParseInt(fields[2], 10, 64)
		branches = append(branches, gitBranchEntry{
			name:      fields[0],
			hash:      fields[1],
			date:      fields[3],
			subject:   fields[4],
			timestamp: timestamp,
		})
	}
	sort.Slice(branches, func(left, right int) bool {
		if branches[left].timestamp == branches[right].timestamp {
			return branches[left].name < branches[right].name
		}
		return branches[left].timestamp > branches[right].timestamp
	})
	return branches, nil
}

func listGitTags(ctx context.Context, repository *gitRepository) ([]gitBranchEntry, error) {
	records, err := listForEachRef(ctx, repository, "refs/tags",
		"%(refname:strip=2)%00%(objecttype)%00%(objectname)%00%(*objectname)%00%(committerdate:unix)%00%(*committerdate:unix)%00%(committerdate:short)%00%(*committerdate:short)%00%(subject)%00%(*subject)")
	if err != nil {
		return nil, err
	}
	tags := make([]gitBranchEntry, 0, len(records))
	for _, fields := range records {
		if len(fields) != 10 {
			return nil, errors.New("git: malformed tag output")
		}
		hash := fields[2]
		if fields[3] != "" {
			hash = fields[3]
		}
		timestampValue := fields[4]
		date := fields[6]
		subject := fields[8]
		if fields[5] != "" {
			timestampValue = fields[5]
		}
		if fields[7] != "" {
			date = fields[7]
		}
		if fields[9] != "" {
			subject = fields[9]
		}
		timestamp, _ := strconv.ParseInt(timestampValue, 10, 64)
		tags = append(tags, gitBranchEntry{
			name:      fields[0],
			hash:      hash,
			date:      date,
			subject:   subject,
			timestamp: timestamp,
		})
	}
	sort.Slice(tags, func(left, right int) bool {
		if tags[left].timestamp == tags[right].timestamp {
			return tags[left].name < tags[right].name
		}
		return tags[left].timestamp > tags[right].timestamp
	})
	return tags, nil
}

func listForEachRef(ctx context.Context, repository *gitRepository, prefix, format string) ([][]string, error) {
	output, truncated, err := captureGitCommand(ctx, gitCommandOptions{
		directory: repository.directory,
		timeout:   gitOperationTimeout,
		secrets:   repository.secrets,
	}, 4*gitMaxResultBytes, "for-each-ref", "--format="+format, prefix)
	if err != nil {
		return nil, fmt.Errorf("git: list refs: %w", err)
	}
	if truncated {
		return nil, errors.New("git: too many refs to list")
	}
	records := make([][]string, 0)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), gitMaxInputLine)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		records = append(records, strings.Split(line, "\x00"))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("git: read refs: %w", err)
	}
	return records, nil
}

func gitDefaultBranch(ctx context.Context, repository *gitRepository) string {
	output, truncated, err := captureGitCommand(ctx, gitCommandOptions{
		directory: repository.directory,
		timeout:   gitOperationTimeout,
		secrets:   repository.secrets,
	}, 256, "symbolic-ref", "--short", "HEAD")
	if err != nil || truncated {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func formatGitBranchLine(entry gitBranchEntry) string {
	marker := "  "
	if entry.defaultBranch {
		marker = "* "
	}
	if entry.date == "" {
		entry.date = "0000-00-00"
	}
	return fmt.Sprintf("%s%s  %s  %s  %s",
		marker,
		truncateGitField(entry.name, gitMaxRefLength),
		shortGitCommit(truncateGitField(entry.hash, 40)),
		truncateGitField(entry.date, 10),
		truncateGitField(entry.subject, 200),
	)
}

func resolveGitCommit(ctx context.Context, repository *gitRepository, ref, revision string) (string, error) {
	value := revision
	if value == "" {
		value = ref
	}
	if value == "" {
		value = "HEAD"
	}
	return resolveGitCommitValue(ctx, repository, value)
}

func resolveGitCommitValue(ctx context.Context, repository *gitRepository, value string) (string, error) {
	if value == "" || hasUnsafeGitControl(value) || strings.HasPrefix(value, "-") {
		return "", errors.New("commit is not available")
	}
	output, truncated, err := captureGitCommand(ctx, gitCommandOptions{
		directory: repository.directory,
		timeout:   gitOperationTimeout,
		secrets:   repository.secrets,
	}, 128, "rev-parse", "--verify", "--end-of-options", value+"^{commit}")
	if err != nil || truncated {
		return "", fmt.Errorf("commit %q is not available", truncateGitField(value, gitMaxRefLength))
	}
	commit := strings.TrimSpace(string(output))
	if !isHexCommit(commit, 40, 40) {
		return "", fmt.Errorf("commit %q is not available", truncateGitField(value, gitMaxRefLength))
	}
	return strings.ToLower(commit), nil
}

func gitObjectType(ctx context.Context, repository *gitRepository, object string) (string, error) {
	output, truncated, err := captureGitCommand(ctx, gitCommandOptions{
		directory: repository.directory,
		timeout:   gitOperationTimeout,
		secrets:   repository.secrets,
	}, 64, "cat-file", "-t", object)
	if err != nil || truncated {
		return "", errors.New("object is not available")
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", errors.New("object is not available")
	}
	return value, nil
}

func pageGitLines(ctx context.Context, repository *gitRepository, args []string, offset, limit, maxOutput int, empty string) (string, error) {
	var output strings.Builder
	skipped := 0
	displayed := 0
	truncated := false
	consume := func(reader io.Reader) (bool, error) {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), gitMaxInputLine)
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
			return false, fmt.Errorf("git: command output line exceeds %d bytes", gitMaxInputLine)
		}
		return false, nil
	}
	if err := runGitCommand(ctx, gitCommandOptions{
		directory: repository.directory,
		timeout:   gitOperationTimeout,
		secrets:   repository.secrets,
	}, consume, args...); err != nil {
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

func readGitSearchMatch(reader *bufio.Reader) (gitSearchMatch, bool, error) {
	filename, terminated, err := readGitDelimited(reader, 0, gitMaxPathLength+64)
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
	text, _, err := readGitDelimited(reader, '\n', gitMaxInputLine)
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
	return truncateRunes(value, maximumRunes)
}

func truncateGitField(value string, maximumRunes int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) || !unicode.IsPrint(character) {
			return ' '
		}
		return character
	}, value)
	return truncateRunes(value, maximumRunes)
}

func truncateRunes(value string, maximumRunes int) string {
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

func shortGitCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
