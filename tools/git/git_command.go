package git

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	gitCloneTimeout                = 90 * time.Second
	gitOperationTimeout            = 20 * time.Second
	gitResourceCheckInterval       = time.Second
	gitRepositoryMaxSize     int64 = gitMaxRepositoryMiB * 1024 * 1024
	gitFreeSpaceReserve      int64 = 512 * 1024 * 1024
)

var (
	errGitResourceLimit = errors.New("git: resource limit exceeded")
	errGitStopWalk      = errors.New("git: stop directory walk")

	authorizationPattern = regexp.MustCompile(`(?i)(authorization\s*:\s*)[^\r\n]+`)
)

type gitCommandOptions struct {
	directory        string
	resourceRoot     string
	diskRoot         string
	timeout          time.Duration
	allowExitCodeOne bool
	username         string
	token            string
	secrets          []string
	config           []string
}

func runGitCommand(ctx context.Context, options gitCommandOptions, consume func(io.Reader) (bool, error), arguments ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkGitResources(options.resourceRoot, options.diskRoot); err != nil {
		return err
	}
	commandContext, cancel := context.WithTimeout(ctx, options.timeout)
	defer cancel()

	commandArguments := append(gitCommandPrefix(), options.config...)
	commandArguments = append(commandArguments, arguments...)
	command := exec.CommandContext(commandContext, "git", commandArguments...)
	command.Dir = options.directory
	command.Env = gitEnvironment(options.username, options.token)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return killGitCommand(command) }
	command.WaitDelay = 2 * time.Second

	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("git: open stdout: %w", err)
	}
	var stderr limitedGitBuffer
	stderr.limit = gitMaxErrorBytes
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("git: start git: %w", redactGitError(err, options.secrets))
	}

	monitorContext, stopMonitor := context.WithCancel(context.Background())
	monitorResult := make(chan error, 1)
	go func() {
		monitorResult <- monitorGitResources(monitorContext, command, options.resourceRoot, options.diskRoot)
	}()

	stopped, consumeErr := consume(stdout)
	if consumeErr != nil || stopped {
		_ = killGitCommand(command)
	}
	waitErr := command.Wait()
	stopMonitor()
	monitorErr := <-monitorResult
	postCheckErr := checkGitResources(options.resourceRoot, options.diskRoot)

	if monitorErr != nil {
		return redactGitError(monitorErr, options.secrets)
	}
	if postCheckErr != nil {
		return redactGitError(postCheckErr, options.secrets)
	}
	if consumeErr != nil {
		return redactGitError(consumeErr, options.secrets)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(commandContext.Err(), context.DeadlineExceeded) {
		return errors.New("git: git command timed out")
	}
	if stopped {
		return nil
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if options.allowExitCodeOne && errors.As(waitErr, &exitError) && exitError.ExitCode() == 1 {
			return nil
		}
		message := strings.TrimSpace(redactGitSecrets(stderr.String(), options.secrets))
		if message == "" {
			return fmt.Errorf("git: git command failed: %w", waitErr)
		}
		return fmt.Errorf("git: git command failed: %s", message)
	}
	return nil
}

func gitCommandPrefix() []string {
	return []string{
		"-c", "credential.helper=",
		"-c", "credential.interactive=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.quotePath=false",
		"-c", "protocol.file.allow=never",
		"-c", "http.followRedirects=false",
		"-c", "color.ui=false",
		"-c", "core.pager=cat",
	}
}

func gitCredentialArguments(username, token string) []string {
	if token == "" {
		return nil
	}
	helper := `!test "$1" = get && printf 'username=%s\npassword=%s\n' "${KRITUI_GIT_USERNAME:-oauth2}" "$KRITUI_GIT_TOKEN"; true`
	return []string{"-c", "credential.helper=" + helper}
}

func gitEnvironment(username, token string) []string {
	environment := make([]string, 0, len(os.Environ())+16)
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(name, "GIT_") || name == "KRITUI_GIT_TOKEN" || name == "KRITUI_GIT_USERNAME" {
			continue
		}
		environment = append(environment, variable)
	}
	if token != "" {
		environment = append(environment, "KRITUI_GIT_TOKEN="+token)
		if strings.TrimSpace(username) != "" {
			environment = append(environment, "KRITUI_GIT_USERNAME="+username)
		}
	}
	return append(environment,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
		"GIT_LFS_SKIP_SMUDGE=1",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_PROTOCOL_FROM_USER=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"LC_ALL=C",
	)
}

func gitSecretVariants(token string) []string {
	if token == "" {
		return nil
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	return []string{token, encoded, "oauth2:" + token, "Basic " + encoded}
}

// gitRedactionValues keeps credentials out of tool output. The username on
// its own is not secret and may legitimately occur in repository content, so
// only credential pairs and token variants are redacted.
func gitRedactionValues(username, token string) []string {
	values := gitSecretVariants(token)
	if username = strings.TrimSpace(username); username != "" && token != "" {
		credentials := username + ":" + token
		values = append(values, credentials, base64.StdEncoding.EncodeToString([]byte(credentials)))
	}
	return values
}

func redactGitSecrets(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return authorizationPattern.ReplaceAllString(value, "${1}[REDACTED]")
}

type redactedGitError struct {
	message string
	cause   error
}

func (err *redactedGitError) Error() string { return err.message }
func (err *redactedGitError) Unwrap() error { return err.cause }

func redactGitError(err error, secrets []string) error {
	if err == nil {
		return nil
	}
	message := redactGitSecrets(err.Error(), secrets)
	if message == err.Error() {
		return err
	}
	return &redactedGitError{message: message, cause: err}
}

func monitorGitResources(ctx context.Context, command *exec.Cmd, sizeRoot, diskRoot string) error {
	ticker := time.NewTicker(gitResourceCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := checkGitResources(sizeRoot, diskRoot); err != nil {
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

func checkGitResources(sizeRoot, diskRoot string) error {
	if sizeRoot != "" {
		size, err := measureGitDirectory(sizeRoot, gitRepositoryMaxSize+1)
		if err != nil {
			return fmt.Errorf("%w: measure repository: %v", errGitResourceLimit, err)
		}
		if size > gitRepositoryMaxSize {
			return fmt.Errorf("%w: repository exceeds %s", errGitResourceLimit, formatGitBytes(uint64(gitRepositoryMaxSize)))
		}
	}
	if diskRoot == "" {
		diskRoot = sizeRoot
	}
	if diskRoot != "" {
		available, err := availableDiskBytes(diskRoot)
		if err != nil {
			return fmt.Errorf("%w: check free disk space: %v", errGitResourceLimit, err)
		}
		if available < uint64(gitFreeSpaceReserve) {
			return fmt.Errorf("%w: free disk space fell below %s", errGitResourceLimit, formatGitBytes(uint64(gitFreeSpaceReserve)))
		}
	}
	return nil
}

func measureGitDirectory(root string, limit int64) (int64, error) {
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
		if total > limit {
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
