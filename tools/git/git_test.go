package git

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"seesharpsi/kritui/tools"
)

const (
	gitFixtureReadmeV1 = "line: one\nneedle here\nNeedle capitalized\nNEEDLE shout\nregex42 alpha\nregex99 beta\n"
	gitFixtureReadmeV2 = "line: one\nneedle here\nNeedle capitalized\nNEEDLE shout\nregex42 alpha\nregex99 beta\nalpha text changed\ndelta text\n"
	gitFixtureMainGo   = "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"
	gitFixtureUtilGo   = "package main\n\nfunc helper() int { return 7 }\n"
	gitFixtureBinary   = "x\x00\x01\x89PNG\x00binary\x00data"
	gitFixtureUnicode  = "unicode: 日本語\n"
)

type gitFixture struct {
	root    string
	work    string
	bare    string
	first   string
	second  string
	feature string
}

func runGitFixture(t *testing.T, dir string, arguments ...string) string {
	t.Helper()
	return runGitFixtureEnv(t, dir, nil, arguments...)
}

func runGitFixtureEnv(t *testing.T, dir string, environment []string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{
		"-c", "safe.directory=*",
		"-c", "user.name=Tester",
		"-c", "user.email=tester@example.com",
		"-c", "commit.gpgsign=false",
	}, arguments...)...)
	command.Dir = dir
	command.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"LC_ALL=C",
	)
	command.Env = append(command.Env, environment...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func gitDateEnvironment(date string) []string {
	return []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	runGitFixture(t, work, "init", "-q", "-b", "main")

	writeFixtureFile(t, filepath.Join(work, "README.md"), gitFixtureReadmeV1)
	for _, directory := range []string{"src", "assets", "docs", "docs/日本語", "docs/日本語/深い"} {
		if err := os.MkdirAll(filepath.Join(work, filepath.FromSlash(directory)), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", directory, err)
		}
	}
	writeFixtureFile(t, filepath.Join(work, "src", "main.go"), gitFixtureMainGo)
	writeFixtureFile(t, filepath.Join(work, "src", "util.go"), gitFixtureUtilGo)
	writeFixtureFile(t, filepath.Join(work, "assets", "logo.bin"), gitFixtureBinary)
	writeFixtureFile(t, filepath.Join(work, "docs", "guide.md"), "# Guide\n")
	writeFixtureFile(t, filepath.Join(work, "docs", "日本語", "メモ.txt"), gitFixtureUnicode)
	writeFixtureFile(t, filepath.Join(work, "docs", "日本語", "深い", "さらに.txt"), "deeper\n")

	runGitFixture(t, work, "add", "-A")
	runGitFixtureEnv(t, work, gitDateEnvironment("2024-01-01T12:00:00+0000"),
		"commit", "-q", "-m", "first commit adding files")
	first := runGitFixture(t, work, "rev-parse", "HEAD")

	writeFixtureFile(t, filepath.Join(work, "README.md"), gitFixtureReadmeV2)
	runGitFixture(t, work, "add", "-A")
	runGitFixtureEnv(t, work, gitDateEnvironment("2024-02-02T12:00:00+0000"),
		"commit", "-q", "-m", "second commit changes readme")
	second := runGitFixture(t, work, "rev-parse", "HEAD")

	runGitFixtureEnv(t, work, gitDateEnvironment("2024-01-05T00:00:00+0000"),
		"tag", "-a", "v1.0.0", "-m", "release one", first)
	runGitFixture(t, work, "tag", "v2.0.0", second)

	runGitFixture(t, work, "checkout", "-q", "-b", "feature", first)
	writeFixtureFile(t, filepath.Join(work, "feature.txt"), "feature work\n")
	runGitFixture(t, work, "add", "-A")
	runGitFixtureEnv(t, work, gitDateEnvironment("2024-03-03T12:00:00+0000"),
		"commit", "-q", "-m", "feature work")
	feature := runGitFixture(t, work, "rev-parse", "HEAD")
	runGitFixture(t, work, "checkout", "-q", "main")

	bare := filepath.Join(base, "bare.git")
	runGitFixture(t, base, "clone", "--bare", "-q", "work", "bare.git")

	return &gitFixture{root: base, work: work, bare: bare, first: first, second: second, feature: feature}
}

func newEmptyGitFixture(t *testing.T) *gitFixture {
	t.Helper()
	base := t.TempDir()
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	runGitFixture(t, work, "init", "-q", "-b", "main")
	runGitFixtureEnv(t, work, gitDateEnvironment("2024-01-01T12:00:00+0000"),
		"commit", "-q", "--allow-empty", "-m", "empty tree")
	empty := runGitFixture(t, work, "rev-parse", "HEAD")
	bare := filepath.Join(base, "empty.git")
	runGitFixture(t, base, "clone", "--bare", "-q", "work", "empty.git")
	return &gitFixture{root: base, work: work, bare: bare, first: empty, second: empty}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (f *gitFixture) repository() *gitRepository {
	return &gitRepository{directory: f.bare, address: "https://git.test/org/repo"}
}

func executeGitTool(t *testing.T, runtime *gitRuntime, operation gitOperation, ctx context.Context, arguments string) (string, error) {
	t.Helper()
	tool := &gitTool{runtime: runtime, operation: operation}
	return tool.Execute(ctx, json.RawMessage(arguments))
}

func executeGitToolInSession(t *testing.T, runtime *gitRuntime, session *GitSession, operation gitOperation, arguments string) (string, error) {
	t.Helper()
	return executeGitTool(t, runtime, operation, session.Context(context.Background()), arguments)
}

func TestGitToolsRegistry(t *testing.T) {
	gitTools := NewGitTools()
	if len(gitTools) != 6 {
		t.Fatalf("NewGitTools() returned %d tools, want 6", len(gitTools))
	}

	registry, err := tools.NewRegistry(gitTools...)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	wantNames := []string{"git_tree", "git_read", "git_search", "git_log", "git_diff", "git_branches"}
	if got := toolNames(registry.Definitions()); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("Definitions() names = %v, want %v", got, wantNames)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"git"}) {
		t.Fatalf("Names() = %v, want [git]", got)
	}
	if !registry.HasCapability("git") {
		t.Fatal("HasCapability(git) = false, want true")
	}

	for index, tool := range gitTools {
		if capability, ok := tool.(tools.CapabilityTool); !ok || capability.Capability() != "git" {
			t.Fatalf("tool %d does not report capability git", index)
		}
		if strings.Contains(string(tool.Definition().Parameters), `"token"`) {
			t.Fatalf("tool %d exposes a token parameter", index)
		}
	}

	selected, err := registry.Select("git")
	if err != nil {
		t.Fatalf("Select(git) error: %v", err)
	}
	if got := toolNames(selected.Definitions()); !reflect.DeepEqual(got, wantNames) {
		t.Fatalf("selected Definitions() names = %v, want %v", got, wantNames)
	}
	if got := selected.Names(); !reflect.DeepEqual(got, []string{"git"}) {
		t.Fatalf("selected Names() = %v, want [git]", got)
	}
}

func TestGitValidationPrecedesSession(t *testing.T) {
	validURL := "https://github.com/octocat/Hello-World"
	tests := []struct {
		name      string
		operation gitOperation
		arguments string
		want      string
	}{
		{name: "tree null arguments", operation: gitTreeOperation, arguments: `null`, want: "arguments must be a JSON object"},
		{name: "tree array arguments", operation: gitTreeOperation, arguments: `[]`, want: "arguments must be a JSON object"},
		{name: "tree trailing object", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q} {}`, validURL), want: "arguments must be a JSON object"},
		{name: "tree unknown field", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"bogus":1}`, validURL), want: "invalid arguments"},
		{name: "tree wrong field type", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"depth":"two"}`, validURL), want: "invalid arguments"},
		{name: "tree null field", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"depth":null}`, validURL), want: `argument "depth" must not be null`},
		{name: "tree missing url", operation: gitTreeOperation, arguments: `{}`, want: "url is required"},
		{name: "tree non-https url", operation: gitTreeOperation, arguments: `{"url":"http://github.com/octocat/Hello-World"}`, want: "must be a fully formed HTTPS URL"},
		{name: "tree userinfo url", operation: gitTreeOperation, arguments: `{"url":"https://user:pass@github.com/octocat/Hello-World"}`, want: "must not contain credentials"},
		{name: "tree query url", operation: gitTreeOperation, arguments: `{"url":"https://github.com/octocat/Hello-World?x=1"}`, want: "must not contain a query or fragment"},
		{name: "tree fragment url", operation: gitTreeOperation, arguments: `{"url":"https://github.com/octocat/Hello-World#readme"}`, want: "must not contain a query or fragment"},
		{name: "tree missing repository path", operation: gitTreeOperation, arguments: `{"url":"https://github.com"}`, want: "must contain a repository path"},
		{name: "tree traversal path", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"../etc/passwd"}`, validURL), want: "must not contain parent traversal"},
		{name: "tree nested traversal path", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"src/../../etc"}`, validURL), want: "must not contain parent traversal"},
		{name: "tree absolute path", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"/etc/passwd"}`, validURL), want: "must be repository-relative"},
		{name: "tree invalid revision", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"revision":"nothex"}`, validURL), want: "revision must be a 7 to 40 character hexadecimal commit hash"},
		{name: "tree negative depth", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"depth":0}`, validURL), want: "depth must be between 1 and 5"},
		{name: "tree oversized depth", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"depth":6}`, validURL), want: "depth must be between 1 and 5"},
		{name: "tree zero limit", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"limit":0}`, validURL), want: "limit must be between 1 and 500"},
		{name: "tree oversized limit", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"limit":501}`, validURL), want: "limit must be between 1 and 500"},
		{name: "read missing url", operation: gitReadOperation, arguments: `{"path":"README.md"}`, want: "url is required"},
		{name: "read missing path", operation: gitReadOperation, arguments: fmt.Sprintf(`{"url":%q}`, validURL), want: "path is required"},
		{name: "read traversal path", operation: gitReadOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"../x"}`, validURL), want: "must not contain parent traversal"},
		{name: "read invalid revision", operation: gitReadOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"README.md","revision":"zzz"}`, validURL), want: "revision must be a 7 to 40 character hexadecimal commit hash"},
		{name: "search missing url", operation: gitSearchOperation, arguments: `{"query":"needle"}`, want: "url is required"},
		{name: "search missing query", operation: gitSearchOperation, arguments: fmt.Sprintf(`{"url":%q}`, validURL), want: "query is required and must not be empty"},
		{name: "search blank query", operation: gitSearchOperation, arguments: fmt.Sprintf(`{"url":%q,"query":"   "}`, validURL), want: "query is required and must not be empty"},
		{name: "search control query", operation: gitSearchOperation, arguments: fmt.Sprintf(`{"url":%q,"query":"a\nb"}`, validURL), want: "must not contain control characters"},
		{name: "search oversized query", operation: gitSearchOperation, arguments: fmt.Sprintf(`{"url":%q,"query":%q}`, validURL, strings.Repeat("a", 513)), want: "query exceeds 512 bytes"},
		{name: "search zero limit", operation: gitSearchOperation, arguments: fmt.Sprintf(`{"url":%q,"query":"needle","limit":0}`, validURL), want: "limit must be between 1 and 100"},
		{name: "log missing url", operation: gitLogOperation, arguments: `{}`, want: "url is required"},
		{name: "log oversized limit", operation: gitLogOperation, arguments: fmt.Sprintf(`{"url":%q,"limit":101}`, validURL), want: "limit must be between 1 and 100"},
		{name: "diff missing url", operation: gitDiffOperation, arguments: `{"base":"abcdef0"}`, want: "url is required"},
		{name: "diff missing base", operation: gitDiffOperation, arguments: fmt.Sprintf(`{"url":%q}`, validURL), want: "base is required"},
		{name: "diff invalid base", operation: gitDiffOperation, arguments: fmt.Sprintf(`{"url":%q,"base":"bad name"}`, validURL), want: "git: base: must be a 7 to 40 character hexadecimal commit or a valid branch/tag name"},
		{name: "diff invalid target", operation: gitDiffOperation, arguments: fmt.Sprintf(`{"url":%q,"base":"abcdef0","target":"bad name"}`, validURL), want: "git: target: must be a 7 to 40 character hexadecimal commit or a valid branch/tag name"},
		{name: "diff zero limit", operation: gitDiffOperation, arguments: fmt.Sprintf(`{"url":%q,"base":"abcdef0","limit":0}`, validURL), want: "limit must be between 1 and 400"},
		{name: "branches zero limit", operation: gitBranchesOperation, arguments: fmt.Sprintf(`{"url":%q,"limit":0}`, validURL), want: "limit must be between 1 and 500"},
		{name: "branches oversized limit", operation: gitBranchesOperation, arguments: fmt.Sprintf(`{"url":%q,"limit":501}`, validURL), want: "limit must be between 1 and 500"},
		{name: "branches null include tags", operation: gitBranchesOperation, arguments: fmt.Sprintf(`{"url":%q,"include_tags":null}`, validURL), want: `argument "include_tags" must not be null`},
		{name: "branches unknown field", operation: gitBranchesOperation, arguments: fmt.Sprintf(`{"url":%q,"ref":"main"}`, validURL), want: "invalid arguments"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := executeGitTool(t, newGitRuntime(), test.operation, context.Background(), test.arguments)
			if err == nil {
				t.Fatalf("Execute(%s) error = nil, want validation error", test.arguments)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute(%s) error = %q, want containing %q", test.arguments, err, test.want)
			}
			if strings.Contains(err.Error(), "session is unavailable") {
				t.Fatalf("Execute(%s) reached session lookup before validation: %q", test.arguments, err)
			}
		})
	}
}

func TestGitValidationRef(t *testing.T) {
	validURL := "https://github.com/octocat/Hello-World"
	tests := []struct {
		name      string
		operation gitOperation
		arguments string
		want      string
	}{
		{name: "tree invalid ref", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"ref":"bad..ref"}`, validURL), want: "ref is not a valid branch or tag"},
		{name: "tree leading dash ref", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"ref":"-oops"}`, validURL), want: "ref is not a valid branch or tag"},
		{name: "tree slash ref", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"ref":"feature//x"}`, validURL), want: "ref is not a valid branch or tag"},
		{name: "tree space ref", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"ref":"bad ref"}`, validURL), want: "ref is not a valid branch or tag"},
		{name: "tree oversized ref", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"ref":%q}`, validURL, strings.Repeat("a", 257)), want: "ref exceeds 256 bytes"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := executeGitTool(t, newGitRuntime(), test.operation, context.Background(), test.arguments)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute(%s) error = %v, want containing %q", test.arguments, err, test.want)
			}
		})
	}
}

func TestGitHostAllowlist(t *testing.T) {
	session := NewGitSession(SessionConfig{
		UserID: 7,
		Hosts: []HostConfig{
			{Host: "Git.Example:8443", Username: "alice", Token: "private-token"},
			{Host: "github.com"},
			{Host: "[2001:db8::1]", Token: "ipv6-token"},
		},
	})

	tests := []struct {
		name         string
		url          string
		wantHost     string
		wantUsername string
		wantToken    string
		wantErr      string
	}{
		{
			name:      "canonical default port",
			url:       "https://GitHub.com:443/Org/Repo",
			wantHost:  "https://github.com/Org/Repo",
			wantToken: "",
		},
		{
			name:         "custom port token",
			url:          "https://git.example:8443/org/repo",
			wantHost:     "https://git.example:8443/org/repo",
			wantUsername: "alice",
			wantToken:    "private-token",
		},
		{
			name:      "ipv6 token",
			url:       "https://[2001:DB8::1]/org/repo",
			wantHost:  "https://[2001:db8::1]/org/repo",
			wantToken: "ipv6-token",
		},
		{name: "wrong custom port", url: "https://git.example:8444/org/repo", wantErr: "not allowed"},
		{name: "unlisted host", url: "https://example.com/org/repo", wantErr: "not allowed"},
		{name: "unlisted default port", url: "https://git.example/org/repo", wantErr: "not allowed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, username, token, err := session.repositoryTarget(test.url)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("repositoryTarget() error = %v, want containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("repositoryTarget() error: %v", err)
			}
			if address != test.wantHost {
				t.Fatalf("repositoryTarget() address = %q, want %q", address, test.wantHost)
			}
			if username != test.wantUsername {
				t.Fatalf("repositoryTarget() username = %q, want %q", username, test.wantUsername)
			}
			if token != test.wantToken {
				t.Fatalf("repositoryTarget() token = %q, want %q", token, test.wantToken)
			}
		})
	}
}

func TestRepositoryURLValidation(t *testing.T) {
	valid := []struct {
		input       string
		wantAddress string
		wantHost    string
	}{
		{input: "https://GitHub.com:443/Org/Repo", wantAddress: "https://github.com/Org/Repo", wantHost: "github.com"},
		{input: "https://git.example:8443/org/repo", wantAddress: "https://git.example:8443/org/repo", wantHost: "git.example:8443"},
		{input: "https://[2001:DB8::1]/org/repo", wantAddress: "https://[2001:db8::1]/org/repo", wantHost: "[2001:db8::1]"},
		{input: "https://[2001:DB8::1]:8443/org/repo", wantAddress: "https://[2001:db8::1]:8443/org/repo", wantHost: "[2001:db8::1]:8443"},
		{input: "https://git.example/org/%E6%97%A5", wantAddress: "https://git.example/org/%E6%97%A5", wantHost: "git.example"},
	}
	for _, test := range valid {
		parsed, err := parseRepositoryURL(test.input)
		if err != nil {
			t.Fatalf("parseRepositoryURL(%q) error: %v", test.input, err)
		}
		if parsed.address != test.wantAddress || parsed.host != test.wantHost {
			t.Fatalf("parseRepositoryURL(%q) = %+v, want address %q host %q", test.input, parsed, test.wantAddress, test.wantHost)
		}
	}

	invalid := []string{
		"",
		"http://git.example/org/repo",
		"ftp://git.example/org/repo",
		"https://user:pass@git.example/org/repo",
		"https://git.example/org/repo?token=x",
		"https://git.example/org/repo#main",
		"https://git.example",
		"https://git.example/",
		"https://git.example/org/repo\n",
		"https://git.example:0/org/repo",
		"https://git.example:99999/org/repo",
		"https://git.example:bad/org/repo",
	}
	for _, address := range invalid {
		if _, err := parseRepositoryURL(address); err == nil {
			t.Fatalf("parseRepositoryURL(%q) error = nil, want rejection", address)
		}
	}
}

func TestHostConfigValidation(t *testing.T) {
	valid := map[string]string{
		"GitHub.com":         "github.com",
		"github.com:443":     "github.com",
		"git.example:8443":   "git.example:8443",
		"[2001:DB8::1]":      "[2001:db8::1]",
		"[2001:db8::1]:8443": "[2001:db8::1]:8443",
		"2001:db8::1":        "[2001:db8::1]",
	}
	for input, want := range valid {
		got, err := parseHostConfig(input)
		if err != nil {
			t.Fatalf("parseHostConfig(%q) error: %v", input, err)
		}
		if got != want {
			t.Fatalf("parseHostConfig(%q) = %q, want %q", input, got, want)
		}
	}
	for _, input := range []string{"", "https://github.com", "github.com/path", "github.com?x", "user@github.com", "github.com:", "bad host"} {
		if _, err := parseHostConfig(input); err == nil {
			t.Fatalf("parseHostConfig(%q) error = nil, want rejection", input)
		}
	}
}

func TestCachePathAndPermissions(t *testing.T) {
	runtime := newGitRuntime()
	runtime.root = filepath.Join(t.TempDir(), "cache")
	accountDirectory, err := runtime.accountDirectory(42)
	if err != nil {
		t.Fatalf("accountDirectory() error: %v", err)
	}
	info, err := os.Stat(accountDirectory)
	if err != nil {
		t.Fatalf("stat account directory: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("account directory mode = %o, want 0700", info.Mode().Perm())
	}
	sum := sha256.Sum256([]byte("https://git.example/org/repo"))
	want := filepath.Join(accountDirectory, hex.EncodeToString(sum[:8]))
	if got := cachePathFor(accountDirectory, "https://git.example/org/repo"); got != want {
		t.Fatalf("cachePathFor() = %q, want %q", got, want)
	}
}

func TestGitSessionContextAndClose(t *testing.T) {
	session := NewGitSession(SessionConfig{UserID: 3})
	if got := session.Context(context.Background()).Value(gitSessionContextKey{}); got != session {
		t.Fatalf("session.Context() value = %v, want session itself", got)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("first Close() error: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close() error: %v, want nil (idempotent)", err)
	}
	var nilSession *GitSession
	if err := nilSession.Close(); err != nil {
		t.Fatalf("nil session Close() error: %v", err)
	}
}

func TestRunGitCommandHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runGitCommand(ctx, gitCommandOptions{timeout: gitOperationTimeout}, discardGitOutput, "version")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runGitCommand() error = %v, want context.Canceled", err)
	}
}

func TestGitToolsRejectAbsentSession(t *testing.T) {
	validURL := "https://github.com/octocat/Hello-World"
	tests := []struct {
		name      string
		operation gitOperation
		arguments string
	}{
		{name: "tree", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q}`, validURL)},
		{name: "read", operation: gitReadOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"README.md"}`, validURL)},
		{name: "search", operation: gitSearchOperation, arguments: fmt.Sprintf(`{"url":%q,"query":"needle"}`, validURL)},
		{name: "log", operation: gitLogOperation, arguments: fmt.Sprintf(`{"url":%q}`, validURL)},
		{name: "diff", operation: gitDiffOperation, arguments: fmt.Sprintf(`{"url":%q,"base":"abcdef0"}`, validURL)},
		{name: "branches", operation: gitBranchesOperation, arguments: fmt.Sprintf(`{"url":%q}`, validURL)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := executeGitTool(t, newGitRuntime(), test.operation, context.Background(), test.arguments)
			if err == nil || !strings.Contains(err.Error(), "git: request session is unavailable") {
				t.Fatalf("Execute() error = %v, want request session unavailable", err)
			}
		})
	}
}

func TestGitTree(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()
	short := fixture.second[:12]

	got, err := gitTree(ctx, repository, "", "", "", 1, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree() error: %v", err)
	}
	want := fmt.Sprintf(". @ %s\n├── README.md\n├── assets/\n├── docs/\n└── src/", short)
	if got != want {
		t.Fatalf("gitTree() = %q, want %q", got, want)
	}

	got, err = gitTree(ctx, repository, "", "", "", 2, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(depth 2) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\n├── README.md\n├── assets/\n│   └── logo.bin\n├── docs/\n│   ├── guide.md\n│   └── 日本語/\n└── src/\n    ├── main.go\n    └── util.go", short)
	if got != want {
		t.Fatalf("gitTree(depth 2) = %q, want %q", got, want)
	}

	got, err = gitTree(ctx, repository, "", "", "docs", 1, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(path) error: %v", err)
	}
	want = fmt.Sprintf("docs/ @ %s\n├── guide.md\n└── 日本語/", short)
	if got != want {
		t.Fatalf("gitTree(path) = %q, want %q", got, want)
	}

	got, err = gitTree(ctx, repository, "", "", "docs", 2, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(unicode depth) error: %v", err)
	}
	for _, wantPart := range []string{"    ├── メモ.txt", "    └── 深い/"} {
		if !strings.Contains(got, wantPart) {
			t.Fatalf("gitTree(unicode depth) = %q, missing %q", got, wantPart)
		}
	}
	if strings.Contains(got, "さらに.txt") {
		t.Fatalf("gitTree(depth 2) traversed too deeply: %q", got)
	}

	got, err = gitTree(ctx, repository, "", "", "docs", 3, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(depth 3) error: %v", err)
	}
	if !strings.Contains(got, "さらに.txt") {
		t.Fatalf("gitTree(depth 3) = %q, missing deeper entry", got)
	}

	got, err = gitTree(ctx, repository, "", "", "", 1, 2, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(paged) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\n├── README.md\n└── assets/\n%s", short, gitTreeTruncation)
	if got != want {
		t.Fatalf("gitTree(paged) = %q, want %q", got, want)
	}
	for _, hint := range []string{"narrower path", "lower depth", "larger limit"} {
		if !strings.Contains(got, hint) {
			t.Fatalf("gitTree(paged) = %q, missing hint %q", got, hint)
		}
	}

	if _, err := gitTree(ctx, repository, "", "", "README.md", 1, 200, gitMaxResultBytes); err == nil || !strings.Contains(err.Error(), "use git_read") {
		t.Fatalf("gitTree(file) error = %v, want use git_read", err)
	}

	got, err = gitTree(ctx, repository, "feature", "", "", 1, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(feature) error: %v", err)
	}
	if !strings.Contains(got, "feature.txt") {
		t.Fatalf("gitTree(feature) = %q, missing feature branch file", got)
	}
}

func TestGitTreeEmptyRoot(t *testing.T) {
	fixture := newEmptyGitFixture(t)
	got, err := gitTree(context.Background(), fixture.repository(), "", "", "", 1, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree() error: %v", err)
	}
	want := fmt.Sprintf(". @ %s\n(empty)", fixture.first[:12])
	if got != want {
		t.Fatalf("gitTree() = %q, want %q", got, want)
	}
}

func TestGitRead(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()

	got, err := gitRead(ctx, repository, "", "", "src/main.go", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitRead() error: %v", err)
	}
	want := fmt.Sprintf("src/main.go @ %s\n     1\tpackage main\n     2\t\n     3\tfunc main() {\n     4\t\tprintln(\"hello\")\n     5\t}", fixture.second[:12])
	if got != want {
		t.Fatalf("gitRead() = %q, want %q", got, want)
	}

	got, err = gitRead(ctx, repository, "", "", "README.md", 3, 3, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitRead(paged) error: %v", err)
	}
	want = fmt.Sprintf("README.md @ %s\n     4\tNEEDLE shout\n     5\tregex42 alpha\n     6\tregex99 beta\n[truncated; next_offset=6]", fixture.second[:12])
	if got != want {
		t.Fatalf("gitRead(paged) = %q, want %q", got, want)
	}

	got, err = gitRead(ctx, repository, "", "", "README.md", 100, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitRead(past end) error: %v", err)
	}
	want = fmt.Sprintf("README.md @ %s\n(no lines at offset 100)", fixture.second[:12])
	if got != want {
		t.Fatalf("gitRead(past end) = %q, want %q", got, want)
	}

	got, err = gitRead(ctx, repository, "", fixture.first, "README.md", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitRead(old revision) error: %v", err)
	}
	want = fmt.Sprintf("README.md @ %s\n     1\tline: one\n     2\tneedle here\n     3\tNeedle capitalized\n     4\tNEEDLE shout\n     5\tregex42 alpha\n     6\tregex99 beta", fixture.first[:12])
	if got != want {
		t.Fatalf("gitRead(old revision) = %q, want %q", got, want)
	}
	if strings.Contains(got, "delta text") {
		t.Fatalf("gitRead(old revision) leaked later content: %q", got)
	}

	got, err = gitRead(ctx, repository, "feature", "", "feature.txt", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitRead(feature) error: %v", err)
	}
	if !strings.Contains(got, "feature work") {
		t.Fatalf("gitRead(feature) = %q, missing feature content", got)
	}

	if _, err := gitRead(ctx, repository, "", "", "assets/logo.bin", 0, 200, gitMaxResultBytes); err == nil || !strings.Contains(err.Error(), "git: file is binary") {
		t.Fatalf("gitRead(binary) error = %v, want binary rejection", err)
	}
	if _, err := gitRead(ctx, repository, "", "", "docs", 0, 200, gitMaxResultBytes); err == nil || !strings.Contains(err.Error(), "use git_tree") {
		t.Fatalf("gitRead(directory) error = %v, want use git_tree", err)
	}
}

func TestGitSearch(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()
	short := fixture.second[:12]

	got, err := gitSearch(ctx, repository, "", "", "needle", "", false, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(literal) error: %v", err)
	}
	want := fmt.Sprintf(". @ %s\nREADME.md:2:needle here\nREADME.md:3:Needle capitalized\nREADME.md:4:NEEDLE shout", short)
	if got != want {
		t.Fatalf("gitSearch(literal) = %q, want %q", got, want)
	}
	if strings.Contains(got, "HEAD:") {
		t.Fatalf("gitSearch(literal) leaked commit prefix: %q", got)
	}

	got, err = gitSearch(ctx, repository, "", "", "needle", "", false, true, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(case sensitive) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\nREADME.md:2:needle here", short)
	if got != want {
		t.Fatalf("gitSearch(case sensitive) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "", "regex[0-9]+", "", true, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(regex) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\nREADME.md:5:regex42 alpha\nREADME.md:6:regex99 beta", short)
	if got != want {
		t.Fatalf("gitSearch(regex) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "", "regex[0-9]+", "", false, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(literal regex string) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\n(no matches)", short)
	if got != want {
		t.Fatalf("gitSearch(literal regex string) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "", "package", "src", false, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(scoped) error: %v", err)
	}
	want = fmt.Sprintf("src @ %s\nsrc/main.go:1:package main\nsrc/util.go:1:package main", short)
	if got != want {
		t.Fatalf("gitSearch(scoped) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "", "nonexistenttoken", "", false, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(no match) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\n(no matches)", short)
	if got != want {
		t.Fatalf("gitSearch(no match) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "", "needle", "", false, false, 0, 2, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(paged) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\nREADME.md:2:needle here\nREADME.md:3:Needle capitalized\n[truncated; next_offset=2]", short)
	if got != want {
		t.Fatalf("gitSearch(paged) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "feature", "", "feature", "", false, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(feature) error: %v", err)
	}
	if !strings.Contains(got, "feature.txt:1:feature work") {
		t.Fatalf("gitSearch(feature) = %q, missing feature match", got)
	}
}

func TestGitLog(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()

	got, err := gitLog(ctx, repository, "", "", "", 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitLog() error: %v", err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("gitLog() = %q, want two commit lines", got)
	}
	want := fmt.Sprintf("%s  2024-02-02  Tester  second commit changes readme", fixture.second[:12])
	if lines[0] != want {
		t.Fatalf("gitLog() first line = %q, want %q", lines[0], want)
	}
	want = fmt.Sprintf("%s  2024-01-01  Tester  first commit adding files", fixture.first[:12])
	if lines[1] != want {
		t.Fatalf("gitLog() second line = %q, want %q", lines[1], want)
	}

	got, err = gitLog(ctx, repository, "", "", "src/main.go", 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitLog(scoped) error: %v", err)
	}
	lines = strings.Split(got, "\n")
	if len(lines) != 1 {
		t.Fatalf("gitLog(scoped) = %q, want one commit line", got)
	}
	if !strings.HasPrefix(lines[0], fixture.first[:12]+"  ") {
		t.Fatalf("gitLog(scoped) line = %q, want first commit only", lines[0])
	}
	if strings.Contains(got, fixture.second[:12]) {
		t.Fatalf("gitLog(scoped) leaked unrelated commit: %q", got)
	}

	got, err = gitLog(ctx, repository, "feature", "", "", 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitLog(feature) error: %v", err)
	}
	lines = strings.Split(got, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], fixture.feature[:12]+"  ") {
		t.Fatalf("gitLog(feature) = %q, want feature commit first", got)
	}
	if !strings.Contains(got, "feature work") {
		t.Fatalf("gitLog(feature) = %q, missing feature subject", got)
	}

	got, err = gitLog(ctx, repository, "", "", "", 0, 1, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitLog(paged) error: %v", err)
	}
	if !strings.HasPrefix(got, fixture.second[:12]+"  ") || !strings.HasSuffix(got, "[truncated; next_offset=1]") {
		t.Fatalf("gitLog(paged) = %q, want newest commit and truncation marker", got)
	}
}

func TestGitDiff(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()

	got, err := gitDiff(ctx, repository, "", fixture.first, fixture.second, "", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitDiff() error: %v", err)
	}
	for _, wantPart := range []string{
		"diff --git a/README.md b/README.md",
		"--- a/README.md",
		"+++ b/README.md",
		"@@ -4,3 +4,5 @@",
		"+alpha text changed",
		"+delta text",
	} {
		if !strings.Contains(got, wantPart) {
			t.Fatalf("gitDiff() = %q, missing %q", got, wantPart)
		}
	}

	got, err = gitDiff(ctx, repository, "", fixture.first, "", "", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitDiff(default target) error: %v", err)
	}
	if !strings.Contains(got, "diff --git a/README.md b/README.md") || !strings.Contains(got, "+alpha text changed") {
		t.Fatalf("gitDiff(default target) = %q, want default to HEAD", got)
	}

	got, err = gitDiff(ctx, repository, "main", "main", "feature", "", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitDiff(named branches) error: %v", err)
	}
	for _, wantPart := range []string{"diff --git a/feature.txt b/feature.txt", "+feature work"} {
		if !strings.Contains(got, wantPart) {
			t.Fatalf("gitDiff(named branches) = %q, missing %q", got, wantPart)
		}
	}

	got, err = gitDiff(ctx, repository, "", fixture.first, fixture.second, "src", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitDiff(scoped) error: %v", err)
	}
	if got != "(no differences)" {
		t.Fatalf("gitDiff(scoped) = %q, want (no differences)", got)
	}

	got, err = gitDiff(ctx, repository, "", fixture.first, fixture.second, "", 0, 4, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitDiff(paged) error: %v", err)
	}
	if !strings.HasPrefix(got, "diff --git a/README.md b/README.md\nindex ") {
		t.Fatalf("gitDiff(paged) = %q, want raw diff start", got)
	}
	if !strings.HasSuffix(got, "[truncated; next_offset=4]") {
		t.Fatalf("gitDiff(paged) = %q, want truncation marker", got)
	}
	if strings.Count(got, "\n") != 4 {
		t.Fatalf("gitDiff(paged) = %q, want 4 lines plus marker", got)
	}

	if _, err := gitDiff(ctx, repository, "", "does-not-exist", "main", "", 0, 200, gitMaxResultBytes); err == nil || !strings.Contains(err.Error(), "resolve base") {
		t.Fatalf("gitDiff(invalid base) error = %v, want resolve base error", err)
	}
}

func TestGitBranches(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()

	got, err := gitBranches(ctx, repository, false, 0, 100, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitBranches() error: %v", err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("gitBranches() = %q, want two branches", got)
	}
	wantFeature := fmt.Sprintf("  feature  %s  2024-03-03  feature work", fixture.feature[:12])
	if lines[0] != wantFeature {
		t.Fatalf("gitBranches() first line = %q, want %q", lines[0], wantFeature)
	}
	wantMain := fmt.Sprintf("* main  %s  2024-02-02  second commit changes readme", fixture.second[:12])
	if lines[1] != wantMain {
		t.Fatalf("gitBranches() second line = %q, want %q", lines[1], wantMain)
	}
	if strings.Contains(got, "* feature") {
		t.Fatalf("gitBranches() marked a non-default branch: %q", got)
	}

	got, err = gitBranches(ctx, repository, true, 0, 100, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitBranches(tags) error: %v", err)
	}
	lines = strings.Split(got, "\n")
	if len(lines) != 5 {
		t.Fatalf("gitBranches(tags) = %q, want five lines", got)
	}
	if lines[2] != "tags:" {
		t.Fatalf("gitBranches(tags) tags line = %q, want tags:", lines[2])
	}
	wantTag := fmt.Sprintf("  v2.0.0  %s  2024-02-02  second commit changes readme", fixture.second[:12])
	if lines[3] != wantTag {
		t.Fatalf("gitBranches(tags) first tag = %q, want %q", lines[3], wantTag)
	}
	wantTag = fmt.Sprintf("  v1.0.0  %s  2024-01-01  first commit adding files", fixture.first[:12])
	if lines[4] != wantTag {
		t.Fatalf("gitBranches(tags) annotated tag = %q, want dereferenced commit %q", lines[4], wantTag)
	}
	tagObject := runGitFixture(t, fixture.work, "rev-parse", "v1.0.0")
	if tagObject != fixture.first && strings.Contains(got, tagObject) {
		t.Fatalf("gitBranches(tags) leaked annotated tag object hash %q: %q", tagObject, got)
	}

	got, err = gitBranches(ctx, repository, true, 0, 2, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitBranches(paged branches) error: %v", err)
	}
	if !strings.HasSuffix(got, "[truncated; next_offset=2]") {
		t.Fatalf("gitBranches(paged branches) = %q, want next_offset=2", got)
	}
	if strings.Contains(got, "tags:") {
		t.Fatalf("gitBranches(paged branches) included unrequested tags: %q", got)
	}

	got, err = gitBranches(ctx, repository, true, 2, 1, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitBranches(paged tags) error: %v", err)
	}
	if !strings.HasPrefix(got, "tags:\n  v2.0.0  ") || !strings.HasSuffix(got, "[truncated; next_offset=3]") {
		t.Fatalf("gitBranches(paged tags) = %q, want tags section and next_offset=3", got)
	}

	got, err = gitBranches(ctx, repository, true, 3, 5, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitBranches(last tag) error: %v", err)
	}
	if !strings.HasPrefix(got, "tags:\n  v1.0.0  ") || strings.Contains(got, "v2.0.0") {
		t.Fatalf("gitBranches(last tag) = %q, want only annotated tag entry", got)
	}
	if strings.Contains(got, "[truncated") {
		t.Fatalf("gitBranches(last tag) = %q, unexpected truncation", got)
	}
}

type gitSyncRecorder struct {
	mu       sync.Mutex
	source   string
	clones   map[string]int
	fetches  map[string]int
	failure  error
	failNext error
}

func (r *gitSyncRecorder) sync(_ context.Context, job gitSyncJob) error {
	r.mu.Lock()
	if r.clones == nil {
		r.clones = make(map[string]int)
	}
	if r.fetches == nil {
		r.fetches = make(map[string]int)
	}
	if job.Clone {
		r.clones[job.URL]++
	} else {
		r.fetches[job.URL]++
	}
	failure := r.failure
	if r.failNext != nil {
		failure = r.failNext
		r.failNext = nil
	}
	source := r.source
	r.mu.Unlock()
	if failure != nil {
		return failure
	}
	if !job.Clone {
		return nil
	}

	if err := os.Remove(job.Directory); err != nil {
		return fmt.Errorf("remove temporary clone: %w", err)
	}
	command := exec.Command("git", "-c", "safe.directory=*", "clone", "--mirror", "--", source, job.Directory)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fixture clone failed: %v: %s", err, output)
	}
	return nil
}

func (r *gitSyncRecorder) counts(url string) (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clones[url], r.fetches[url]
}

func (r *gitSyncRecorder) setFailure(err error) {
	r.mu.Lock()
	r.failure = err
	r.mu.Unlock()
}

type fakeClock struct {
	mu      sync.Mutex
	current time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{current: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (clock *fakeClock) now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = clock.current.Add(time.Minute)
	return clock.current
}

func newCacheTestRuntime(t *testing.T, source string) (*gitRuntime, *gitSyncRecorder) {
	t.Helper()
	recorder := &gitSyncRecorder{source: source}
	runtime := newGitRuntime()
	runtime.root = t.TempDir()
	runtime.syncRepository = recorder.sync
	runtime.minFreeBytes = 0
	clock := newFakeClock()
	runtime.now = clock.now
	return runtime, recorder
}

func testSession(userID int64, hosts ...HostConfig) *GitSession {
	return NewGitSession(SessionConfig{UserID: userID, Hosts: hosts})
}

func TestGitCacheReuseAndFetchOncePerSession(t *testing.T) {
	fixture := newGitFixture(t)
	runtime, recorder := newCacheTestRuntime(t, fixture.bare)
	host := HostConfig{Host: "git.test"}
	address := "https://git.test/org/repo"
	session := testSession(7, host)

	if _, err := executeGitToolInSession(t, runtime, session, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, address)); err != nil {
		t.Fatalf("tree error: %v", err)
	}
	clones, fetches := recorder.counts(address)
	if clones != 1 || fetches != 0 {
		t.Fatalf("first use clones=%d fetches=%d, want clone once", clones, fetches)
	}
	if _, err := executeGitToolInSession(t, runtime, session, gitLogOperation, fmt.Sprintf(`{"url":%q}`, address)); err != nil {
		t.Fatalf("log error: %v", err)
	}
	clones, fetches = recorder.counts(address)
	if clones != 1 || fetches != 0 {
		t.Fatalf("same session clones=%d fetches=%d, want no new sync", clones, fetches)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}

	accountDirectory, err := runtime.accountDirectory(7)
	if err != nil {
		t.Fatalf("accountDirectory() error: %v", err)
	}
	cachePath := cachePathFor(accountDirectory, address)
	if info, err := os.Stat(cachePath); err != nil || !info.IsDir() {
		t.Fatalf("cache entry missing after Close(): %v", err)
	}

	second := testSession(7, host)
	if _, err := executeGitToolInSession(t, runtime, second, gitSearchOperation, fmt.Sprintf(`{"url":%q,"query":"needle"}`, address)); err != nil {
		t.Fatalf("second session error: %v", err)
	}
	clones, fetches = recorder.counts(address)
	if clones != 1 || fetches != 1 {
		t.Fatalf("second session clones=%d fetches=%d, want one fetch", clones, fetches)
	}
	if _, err := executeGitToolInSession(t, runtime, second, gitReadOperation, fmt.Sprintf(`{"url":%q,"path":"README.md"}`, address)); err != nil {
		t.Fatalf("second session second read error: %v", err)
	}
	clones, fetches = recorder.counts(address)
	if clones != 1 || fetches != 1 {
		t.Fatalf("second session repeated use clones=%d fetches=%d, want one fetch", clones, fetches)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close() error: %v", err)
	}

	third := testSession(7, host)
	if _, err := executeGitToolInSession(t, runtime, third, gitBranchesOperation, fmt.Sprintf(`{"url":%q}`, address)); err != nil {
		t.Fatalf("third session error: %v", err)
	}
	clones, fetches = recorder.counts(address)
	if clones != 1 || fetches != 2 {
		t.Fatalf("third session clones=%d fetches=%d, want fetch per session", clones, fetches)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("third Close() error: %v", err)
	}

	otherRuntime, otherRecorder := newCacheTestRuntime(t, fixture.bare)
	otherRuntime.root = runtime.root
	fourth := testSession(7, host)
	if _, err := executeGitToolInSession(t, otherRuntime, fourth, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, address)); err != nil {
		t.Fatalf("fourth session error: %v", err)
	}
	clones, fetches = otherRecorder.counts(address)
	if clones != 0 || fetches != 1 {
		t.Fatalf("cross-runtime session clones=%d fetches=%d, want cache reuse via fetch", clones, fetches)
	}
	if err := fourth.Close(); err != nil {
		t.Fatalf("fourth Close() error: %v", err)
	}
}

func TestGitCacheCorruptEntryRecloned(t *testing.T) {
	fixture := newGitFixture(t)
	runtime, recorder := newCacheTestRuntime(t, fixture.bare)
	host := HostConfig{Host: "git.test"}
	address := "https://git.test/org/repo"

	session := testSession(9, host)
	if _, err := executeGitToolInSession(t, runtime, session, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, address)); err != nil {
		t.Fatalf("tree error: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	accountDirectory, err := runtime.accountDirectory(9)
	if err != nil {
		t.Fatalf("accountDirectory() error: %v", err)
	}
	cachePath := cachePathFor(accountDirectory, address)
	if err := os.Remove(filepath.Join(cachePath, "HEAD")); err != nil {
		t.Fatalf("remove HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cachePath, "garbage"), []byte("not a repository"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	second := testSession(9, host)
	if _, err := executeGitToolInSession(t, runtime, second, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, address)); err != nil {
		t.Fatalf("tree after corruption error: %v", err)
	}
	clones, fetches := recorder.counts(address)
	if clones != 2 || fetches != 0 {
		t.Fatalf("corrupt cache clones=%d fetches=%d, want one reclone", clones, fetches)
	}
	if _, err := os.Stat(filepath.Join(cachePath, "HEAD")); err != nil {
		t.Fatalf("recloned cache missing HEAD: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
}

func TestGitCacheEvictsLeastRecentlyUsed(t *testing.T) {
	fixture := newGitFixture(t)
	runtime, recorder := newCacheTestRuntime(t, fixture.bare)
	runtime.maxEntries = 2
	host := HostConfig{Host: "git.test"}
	addresses := []string{
		"https://git.test/org/one",
		"https://git.test/org/two",
		"https://git.test/org/three",
	}
	for _, address := range addresses {
		session := testSession(11, host)
		if _, err := executeGitToolInSession(t, runtime, session, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, address)); err != nil {
			t.Fatalf("tree %s error: %v", address, err)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("Close() error: %v", err)
		}
	}
	accountDirectory, err := runtime.accountDirectory(11)
	if err != nil {
		t.Fatalf("accountDirectory() error: %v", err)
	}
	if _, err := os.Stat(cachePathFor(accountDirectory, addresses[0])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest cache entry still exists: %v", err)
	}
	for _, address := range addresses[1:] {
		if info, err := os.Stat(cachePathFor(accountDirectory, address)); err != nil || !info.IsDir() {
			t.Fatalf("recent cache entry %s missing: %v", address, err)
		}
	}
	totalClones, _ := recorder.counts(addresses[2])
	if totalClones != 1 {
		t.Fatalf("third repository clones = %d, want 1", totalClones)
	}

	runtime.maxEntries = 2
	first := testSession(12, host)
	second := testSession(12, host)
	if _, err := executeGitToolInSession(t, runtime, first, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, addresses[0])); err != nil {
		t.Fatalf("pinned first error: %v", err)
	}
	if _, err := executeGitToolInSession(t, runtime, second, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, addresses[1])); err != nil {
		t.Fatalf("pinned second error: %v", err)
	}
	third := testSession(12, host)
	_, err = executeGitToolInSession(t, runtime, third, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, addresses[2]))
	if err == nil || !strings.Contains(err.Error(), "cache is full") || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("eviction error = %v, want clear cache in-use error", err)
	}
	_ = first.Close()
	_ = second.Close()
	_ = third.Close()
}

func TestGitTokensNeverLeak(t *testing.T) {
	const token = "super-secret-token-value"
	const username = "configured-git-user"
	encoded := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	userEncoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
	fixture := newGitFixture(t)
	runtime, recorder := newCacheTestRuntime(t, fixture.bare)
	host := HostConfig{Host: "git.test", Username: username, Token: token}
	address := "https://git.test/org/repo"

	recorder.setFailure(fmt.Errorf("clone failed token=%s user=%s basic=%s userbasic=%s header=Authorization: Basic %s", token, username, encoded, userEncoded, userEncoded))
	session := testSession(21, host)
	_, err := executeGitToolInSession(t, runtime, session, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, address))
	if err == nil {
		t.Fatal("Execute() error = nil, want injected failure")
	}
	message := err.Error()
	for _, secret := range []string{token, encoded, userEncoded, "Basic " + userEncoded} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked secret %q: %q", secret, message)
		}
	}
	if !strings.Contains(message, "[REDACTED]") {
		t.Fatalf("error = %q, want [REDACTED]", message)
	}
	_ = session.Close()

	successRuntime, _ := newCacheTestRuntime(t, fixture.bare)
	successSession := testSession(22, host)
	if _, err := executeGitToolInSession(t, successRuntime, successSession, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, address)); err != nil {
		t.Fatalf("tree error: %v", err)
	}
	result, err := successRuntime.withRepository(successSession.Context(context.Background()), address, func(*gitRepository) (string, error) {
		return "result " + token + " user " + username, nil
	})
	if err != nil {
		t.Fatalf("withRepository() error: %v", err)
	}
	if !strings.Contains(result, username) {
		t.Fatalf("tool result = %q, want ordinary username preserved", result)
	}
	if strings.Contains(result, token) {
		t.Fatalf("tool result leaked token: %q", result)
	}
	if !strings.Contains(result, "[REDACTED]") {
		t.Fatalf("tool result = %q, want redacted credentials", result)
	}
	_, err = successRuntime.withRepository(successSession.Context(context.Background()), address, func(*gitRepository) (string, error) {
		return "", errors.New("operation " + token + " user " + username)
	})
	if err == nil {
		t.Fatal("operation error = nil, want injected error")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("operation error leaked token: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("operation error = %v, want [REDACTED]", err)
	}
	_ = successSession.Close()
}

func TestGitCredentialEnvironmentAndRedaction(t *testing.T) {
	const token = "credential-token"
	const username = "credential-user"
	arguments := gitCredentialArguments(username, token)
	joined := strings.Join(arguments, " ")
	for _, secret := range []string{token, username} {
		if strings.Contains(joined, secret) {
			t.Fatalf("credential arguments contain %q: %q", secret, joined)
		}
	}
	for _, required := range []string{"$KRITUI_GIT_TOKEN", "KRITUI_GIT_USERNAME:-oauth2", "oauth2", `test "$1" = get`} {
		if !strings.Contains(joined, required) {
			t.Fatalf("credential arguments = %q, missing %q", joined, required)
		}
	}
	if got := gitCredentialArguments(username, ""); got != nil {
		t.Fatalf("gitCredentialArguments(empty token) = %v, want nil", got)
	}

	t.Setenv("GIT_DIR", "/tmp/evil")
	t.Setenv("KRITUI_GIT_TOKEN", "inherited-token")
	t.Setenv("KRITUI_GIT_USERNAME", "inherited-user")
	environment := strings.Join(gitEnvironment(username, token), "\n")
	for _, disallowed := range []string{
		"GIT_DIR=/tmp/evil",
		"KRITUI_GIT_TOKEN=inherited-token",
		"KRITUI_GIT_USERNAME=inherited-user",
	} {
		if strings.Contains(environment, disallowed) {
			t.Fatalf("environment leaked %q", disallowed)
		}
	}
	for _, required := range []string{
		"KRITUI_GIT_TOKEN=" + token,
		"KRITUI_GIT_USERNAME=" + username,
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	} {
		if !strings.Contains(environment, required) {
			t.Fatalf("environment missing %q", required)
		}
	}
	blankUsernameEnvironment := strings.Join(gitEnvironment("", token), "\n")
	if strings.Contains(blankUsernameEnvironment, "KRITUI_GIT_USERNAME=") {
		t.Fatalf("blank username should leave KRITUI_GIT_USERNAME unset: %q", blankUsernameEnvironment)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte("oauth2:" + token))
	redacted := redactGitSecrets("token="+token+" encoded="+encoded+" Authorization: Bearer abc.def", gitSecretVariants(token))
	for _, secret := range []string{token, encoded, "abc.def"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("redactGitSecrets leaked %q: %q", secret, redacted)
		}
	}
}

func TestGitCredentialHelperUsernameSelection(t *testing.T) {
	const token = "helper-token"
	arguments := gitCredentialArguments("alice", token)
	if len(arguments) != 2 {
		t.Fatalf("gitCredentialArguments() = %v, want two arguments", arguments)
	}
	helper := strings.TrimPrefix(arguments[1], "credential.helper=")
	helper = strings.TrimPrefix(helper, "!")
	if helper == arguments[1] {
		t.Fatalf("credential helper = %q, want shell helper", arguments[1])
	}

	run := func(t *testing.T, environment []string) string {
		t.Helper()
		command := exec.Command("sh", "-c", helper, "git-credential-helper", "get")
		command.Env = append([]string{"KRITUI_GIT_TOKEN=" + token}, environment...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("credential helper failed: %v\n%s", err, output)
		}
		return strings.TrimSpace(string(output))
	}

	got := run(t, []string{"KRITUI_GIT_USERNAME=alice"})
	if got != "username=alice\npassword="+token {
		t.Fatalf("credential helper output = %q, want selected username", got)
	}
	got = run(t, nil)
	if got != "username=oauth2\npassword="+token {
		t.Fatalf("credential helper output = %q, want oauth2 fallback", got)
	}
}

func TestGitValidationDisallowedHost(t *testing.T) {
	runtime, _ := newCacheTestRuntime(t, "")
	session := testSession(1, HostConfig{Host: "github.com"})
	tests := []struct {
		name string
		url  string
	}{
		{name: "unlisted host", url: "https://example.com/org/repo"},
		{name: "unlisted custom port", url: "https://github.com:8443/org/repo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := executeGitToolInSession(t, runtime, session, gitTreeOperation, fmt.Sprintf(`{"url":%q}`, test.url))
			if err == nil || !strings.Contains(err.Error(), "not allowed") {
				t.Fatalf("Execute() error = %v, want host not allowed", err)
			}
		})
	}
	_ = session.Close()
}

func TestGitBranchesEmptyRepository(t *testing.T) {
	base := t.TempDir()
	bare := filepath.Join(base, "empty.git")
	runGitFixture(t, base, "init", "--bare", "-q", "empty.git")
	repository := &gitRepository{directory: bare, address: "https://git.test/org/empty"}
	got, err := gitBranches(context.Background(), repository, false, 0, 100, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitBranches() error: %v", err)
	}
	if got != "(no branches)" {
		t.Fatalf("gitBranches() = %q, want (no branches)", got)
	}
}

func toolNames(definitions []tools.Definition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}
