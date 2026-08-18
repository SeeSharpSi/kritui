package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type gitTreeEntry struct {
	name       string
	objectType string
}

type gitTreeNode struct {
	name       string
	objectType string
	children   []*gitTreeNode
	byName     map[string]*gitTreeNode
}

type gitSearchMatch struct {
	path string
	line int
	text string
}

func gitTree(ctx context.Context, repository *gitRepository, revision, repositoryPath string, recursive bool, offset, limit, maxOutput int) (string, error) {
	commit, err := resolveGitCommit(ctx, repository, revision)
	if err != nil {
		return "", err
	}
	treeish := commit
	if repositoryPath != "" {
		treeish += ":" + repositoryPath
	}
	args := []string{"ls-tree", "-z"}
	if recursive {
		args = append(args, "-r", "-t")
	}
	args = append(args, treeish)

	entries := make([]gitTreeEntry, 0, min(limit+1, 501))
	skipped := 0
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
			entries = append(entries, entry)
			if len(entries) > limit {
				return true, nil
			}
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("git: read tree: %w", err)
		}
		return false, nil
	}
	if err := runGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
	}, consume, args...); err != nil {
		return "", fmt.Errorf("git: list tree: %w", err)
	}

	truncated := len(entries) > limit
	if truncated {
		entries = entries[:limit]
	}
	rootLabel := "."
	if repositoryPath != "" {
		rootLabel = printableGitPath(repositoryPath) + "/"
	}
	rootLabel += " @ " + shortGitCommit(commit)
	if len(entries) == 0 && offset > 0 {
		return fmt.Sprintf("%s\n(no entries at offset %d)", rootLabel, offset), nil
	}

	low, high := 0, len(entries)
	best := rootLabel + "\n(empty)"
	bestCount := 0
	for low <= high {
		count := low + (high-low)/2
		formatted := formatGitTree(rootLabel, entries[:count])
		reserve := 0
		if count < len(entries) || truncated {
			reserve = 64
		}
		if len(formatted)+reserve <= maxOutput {
			best = formatted
			bestCount = count
			low = count + 1
		} else {
			high = count - 1
		}
	}
	if bestCount < len(entries) {
		truncated = true
	}
	if truncated {
		var output strings.Builder
		output.WriteString(best)
		output.WriteByte('\n')
		appendGitTruncation(&output, offset+bestCount)
		return strings.TrimRight(output.String(), "\n"), nil
	}
	return best, nil
}

func gitRead(ctx context.Context, repository *gitRepository, revision, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	commit, err := resolveGitCommit(ctx, repository, revision)
	if err != nil {
		return "", err
	}
	header := fmt.Sprintf("%s @ %s", printableGitPath(repositoryPath), shortGitCommit(commit))
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
			if len(header)+1+body.Len()+len(formatted)+64 > maxOutput {
				truncated = true
				return true, nil
			}
			body.WriteString(formatted)
			displayed++
		}
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("git: source line exceeds %d bytes", gitMaxInputLineBytes)
		}
		return false, nil
	}
	if err := runGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
	}, consume, "cat-file", "blob", commit+":"+repositoryPath); err != nil {
		return "", fmt.Errorf("git: read %q: %w", repositoryPath, err)
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

func gitSearch(ctx context.Context, repository *gitRepository, revision, query, repositoryPath string, regex, caseSensitive bool, offset, limit, maxOutput int) (string, error) {
	commit, err := resolveGitCommit(ctx, repository, revision)
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
		resourceRoot:     repository.root,
		timeout:          gitOperationTimeout,
		allowExitCodeOne: true,
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

func gitLog(ctx context.Context, repository *gitRepository, revision, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	commit, err := resolveGitCommit(ctx, repository, revision)
	if err != nil {
		return "", err
	}
	args := []string{
		"log",
		"-z",
		"--no-decorate",
		"--format=%H%x00%aI%x00%an%x00%s",
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
			line := fmt.Sprintf("%s  %s  %s  %s\n",
				shortGitCommit(fields[0]),
				truncateGitText(fields[1], 40),
				truncateGitText(fields[2], 80),
				truncateGitText(fields[3], 300),
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
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
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

func gitDiff(ctx context.Context, repository *gitRepository, base, target, repositoryPath string, offset, limit, maxOutput int) (string, error) {
	baseCommit, err := resolveGitCommit(ctx, repository, base)
	if err != nil {
		return "", fmt.Errorf("git: resolve base: %w", err)
	}
	targetCommit, err := resolveGitCommit(ctx, repository, target)
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

func resolveGitCommit(ctx context.Context, repository *gitRepository, revision string) (string, error) {
	if revision == "" {
		if repository.commit != "" {
			return repository.commit, nil
		}
		revision = "HEAD"
	}
	output, truncated, err := captureGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
	}, 128, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", errors.New("commit is unavailable in the selected branch")
	}
	if truncated {
		return "", errors.New("git: resolve commit: unexpected output")
	}
	commit := strings.TrimSpace(string(output))
	if !isHexCommit(commit, 40, 40) {
		return "", errors.New("git: resolve commit: invalid hash")
	}
	return strings.ToLower(commit), nil
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
	if err := runGitCommand(ctx, gitCommandOptions{
		directory:    repository.directory,
		resourceRoot: repository.root,
		timeout:      gitOperationTimeout,
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

func formatGitTree(rootLabel string, entries []gitTreeEntry) string {
	root := &gitTreeNode{byName: make(map[string]*gitTreeNode)}
	for _, entry := range entries {
		current := root
		components := strings.Split(entry.name, "/")
		for index, component := range components {
			child := current.byName[component]
			if child == nil {
				child = &gitTreeNode{name: component, byName: make(map[string]*gitTreeNode)}
				current.byName[component] = child
				current.children = append(current.children, child)
			}
			if index == len(components)-1 {
				child.objectType = entry.objectType
			}
			current = child
		}
	}

	var output strings.Builder
	output.WriteString(rootLabel)
	output.WriteByte('\n')
	if len(entries) == 0 {
		output.WriteString("(empty)")
		return output.String()
	}
	writeGitTreeNodes(&output, root.children, "")
	return strings.TrimRight(output.String(), "\n")
}

func writeGitTreeNodes(output *strings.Builder, nodes []*gitTreeNode, prefix string) {
	for index, node := range nodes {
		last := index == len(nodes)-1
		connector := "|-- "
		childPrefix := prefix + "|   "
		if last {
			connector = "`-- "
			childPrefix = prefix + "    "
		}
		output.WriteString(prefix)
		output.WriteString(connector)
		output.WriteString(printableGitTreeName(node.name))
		if node.objectType == "tree" || len(node.children) > 0 {
			output.WriteByte('/')
		} else if node.objectType == "commit" {
			output.WriteString(" [submodule]")
		}
		output.WriteByte('\n')
		writeGitTreeNodes(output, node.children, childPrefix)
	}
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

func shortGitCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
