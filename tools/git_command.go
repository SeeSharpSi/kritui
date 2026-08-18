package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	errGitResourceLimit = errors.New("git: resource limit exceeded")
	errGitStopWalk      = errors.New("git: stop directory walk")
)

type gitCommandOptions struct {
	directory        string
	resourceRoot     string
	timeout          time.Duration
	allowExitCodeOne bool
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
	command.Cancel = func() error { return killGitCommand(command) }
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
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
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
