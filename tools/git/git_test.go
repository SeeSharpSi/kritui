package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"seesharpsi/kritui/tools"
)

const (
	gitFixtureReadmeV1 = "line: one\nneedle here\nNeedle capitalized\nNEEDLE shout\nregex42 alpha\nregex99 beta\n"
	gitFixtureReadmeV2 = "line: one\nneedle here\nNeedle capitalized\nNEEDLE shout\nregex42 alpha\nregex99 beta\nalpha text changed\ndelta text\n"
	gitFixtureMainGo   = "package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"
	gitFixtureUtilGo   = "package main\n\nfunc helper() int { return 7 }\n"
	gitFixtureBinary   = "x\x00\x01\x89PNG\x00binary\x00data"
)

type gitFixture struct {
	root   string
	bare   string
	first  string
	second string
}

func runGitFixture(t *testing.T, dir string, arguments ...string) string {
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
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
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
	if err := os.MkdirAll(filepath.Join(work, "src"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(work, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	writeFixtureFile(t, filepath.Join(work, "src", "main.go"), gitFixtureMainGo)
	writeFixtureFile(t, filepath.Join(work, "src", "util.go"), gitFixtureUtilGo)
	writeFixtureFile(t, filepath.Join(work, "assets", "logo.bin"), gitFixtureBinary)

	runGitFixture(t, work, "add", "-A")
	runGitFixture(t, work, "commit", "-q", "-m", "first commit adding files")
	first := runGitFixture(t, work, "rev-parse", "HEAD")

	writeFixtureFile(t, filepath.Join(work, "README.md"), gitFixtureReadmeV2)
	runGitFixture(t, work, "add", "-A")
	runGitFixture(t, work, "commit", "-q", "-m", "second commit changes readme")
	second := runGitFixture(t, work, "rev-parse", "HEAD")

	bare := filepath.Join(base, "bare.git")
	runGitFixture(t, base, "clone", "--bare", "-q", "work", "bare.git")

	return &gitFixture{root: base, bare: bare, first: first, second: second}
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (f *gitFixture) repository() *gitRepository {
	return &gitRepository{
		root:      f.bare,
		directory: f.bare,
		commit:    f.second,
	}
}

func executeGitTool(t *testing.T, operation gitOperation, ctx context.Context, arguments string) (string, error) {
	t.Helper()
	tool := &gitTool{runtime: newGitRuntime(), operation: operation}
	return tool.Execute(ctx, json.RawMessage(arguments))
}

func TestGitToolsRegistry(t *testing.T) {
	gitTools := NewGitTools()
	if len(gitTools) != 5 {
		t.Fatalf("NewGitTools() returned %d tools, want 5", len(gitTools))
	}

	registry, err := tools.NewRegistry(gitTools...)
	if err != nil {
		t.Fatalf("NewRegistry() error: %v", err)
	}

	wantNames := []string{"git_tree", "git_read", "git_search", "git_log", "git_diff"}
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
		{name: "tree wrong field type", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"recursive":"yes"}`, validURL), want: "invalid arguments"},
		{name: "tree null field", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"recursive":null}`, validURL), want: `argument "recursive" must not be null`},
		{name: "tree missing url", operation: gitTreeOperation, arguments: `{}`, want: "url is required"},
		{name: "tree non-https url", operation: gitTreeOperation, arguments: `{"url":"http://github.com/octocat/Hello-World"}`, want: "must be a fully formed HTTPS URL"},
		{name: "tree disallowed host", operation: gitTreeOperation, arguments: `{"url":"https://example.com/octocat/Hello-World"}`, want: "host must be github.com"},
		{name: "tree traversal path", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"../etc/passwd"}`, validURL), want: "must not contain parent traversal"},
		{name: "tree nested traversal path", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"src/../../etc"}`, validURL), want: "must not contain parent traversal"},
		{name: "tree absolute path", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"path":"/etc/passwd"}`, validURL), want: "must be repository-relative"},
		{name: "tree invalid revision", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"revision":"nothex"}`, validURL), want: "revision must be a 7 to 40 character hexadecimal commit hash"},
		{name: "tree short revision", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"revision":"abc"}`, validURL), want: "revision must be a 7 to 40 character hexadecimal commit hash"},
		{name: "tree negative offset", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"offset":-1}`, validURL), want: "offset must be between 0 and 100000"},
		{name: "tree oversized offset", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"offset":100001}`, validURL), want: "offset must be between 0 and 100000"},
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
		{name: "diff invalid base", operation: gitDiffOperation, arguments: fmt.Sprintf(`{"url":%q,"base":"bad!"}`, validURL), want: "git: base: revision must be a 7 to 40 character hexadecimal commit hash"},
		{name: "diff invalid target", operation: gitDiffOperation, arguments: fmt.Sprintf(`{"url":%q,"base":"abcdef0","target":"nothex"}`, validURL), want: "git: target: revision must be a 7 to 40 character hexadecimal commit hash"},
		{name: "diff zero limit", operation: gitDiffOperation, arguments: fmt.Sprintf(`{"url":%q,"base":"abcdef0","limit":0}`, validURL), want: "limit must be between 1 and 400"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := executeGitTool(t, test.operation, context.Background(), test.arguments)
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
	session := NewGitSession()
	ctx := session.Context(context.Background())
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
		{name: "tree oversized ref", operation: gitTreeOperation, arguments: fmt.Sprintf(`{"url":%q,"ref":%q}`, validURL, strings.Repeat("a", 257)), want: "ref exceeds 256 bytes"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := executeGitTool(t, test.operation, ctx, test.arguments)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute(%s) error = %v, want containing %q", test.arguments, err, test.want)
			}
		})
	}
}

func TestGitSessionContextAndClose(t *testing.T) {
	session := NewGitSession()
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

func TestGitSessionCloseRemovesRepositories(t *testing.T) {
	runtime := newGitRuntime()
	root := filepath.Join(t.TempDir(), "snapshot")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	runtime.active = 1
	repository := &gitRepository{runtime: runtime, root: root, directory: root}
	session := NewGitSession()
	session.repositories[runtime] = map[string]*gitRepository{"repository": repository}

	if err := session.Close(); err != nil {
		t.Fatalf("Close() error: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot still exists after Close(): %v", err)
	}
	if runtime.active != 0 {
		t.Fatalf("runtime active = %d, want 0", runtime.active)
	}
}

func TestGitSessionRetriesFailedCleanup(t *testing.T) {
	runtime := newGitRuntime()
	root := filepath.Join(t.TempDir(), "snapshot")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}
	runtime.active = 1
	removeCalls := 0
	runtime.removeAll = func(path string) error {
		removeCalls++
		if removeCalls == 1 {
			return errors.New("injected cleanup failure")
		}
		return os.RemoveAll(path)
	}
	repository := &gitRepository{runtime: runtime, root: root, directory: root}
	session := NewGitSession()
	session.repositories[runtime] = map[string]*gitRepository{"repository": repository}

	if err := session.Close(); err == nil || !strings.Contains(err.Error(), "injected cleanup failure") {
		t.Fatalf("Close() error = %v, want injected cleanup failure", err)
	}
	if runtime.active != 1 {
		t.Fatalf("runtime active after failed cleanup = %d, want 1", runtime.active)
	}
	if _, exists := runtime.orphans[root]; !exists {
		t.Fatal("failed cleanup root was not retained for retry")
	}

	runtime.retryOrphans()
	if runtime.active != 0 {
		t.Fatalf("runtime active after retry = %d, want 0", runtime.active)
	}
	if len(runtime.orphans) != 0 {
		t.Fatalf("orphans after retry = %v, want empty", runtime.orphans)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot still exists after retry: %v", err)
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
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := executeGitTool(t, test.operation, context.Background(), test.arguments)
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

	got, err := gitTree(ctx, repository, "", "", false, 0, 100, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree() error: %v", err)
	}
	want := fmt.Sprintf(". @ %s\n|-- README.md\n|-- assets/\n`-- src/", short)
	if got != want {
		t.Fatalf("gitTree() = %q, want %q", got, want)
	}
	if strings.Contains(got, "[") && !strings.Contains(got, "[truncated") {
		t.Fatalf("gitTree() looks like JSON: %q", got)
	}

	got, err = gitTree(ctx, repository, "", "", true, 0, 100, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(recursive) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\n|-- README.md\n|-- assets/\n|   `-- logo.bin\n`-- src/\n    |-- main.go\n    `-- util.go", short)
	if got != want {
		t.Fatalf("gitTree(recursive) = %q, want %q", got, want)
	}

	got, err = gitTree(ctx, repository, "", "src", false, 0, 100, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(path) error: %v", err)
	}
	want = fmt.Sprintf("src/ @ %s\n|-- main.go\n`-- util.go", short)
	if got != want {
		t.Fatalf("gitTree(path) = %q, want %q", got, want)
	}

	got, err = gitTree(ctx, repository, "", "", false, 0, 2, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitTree(paged) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\n|-- README.md\n`-- assets/\n[truncated; next_offset=2]", short)
	if got != want {
		t.Fatalf("gitTree(paged) = %q, want %q", got, want)
	}
}

func TestGitRead(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()

	got, err := gitRead(ctx, repository, "", "src/main.go", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitRead() error: %v", err)
	}
	want := fmt.Sprintf("src/main.go @ %s\n     1\tpackage main\n     2\t\n     3\tfunc main() {\n     4\t\tprintln(\"hello\")\n     5\t}", fixture.second[:12])
	if got != want {
		t.Fatalf("gitRead() = %q, want %q", got, want)
	}

	got, err = gitRead(ctx, repository, "", "README.md", 3, 3, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitRead(paged) error: %v", err)
	}
	want = fmt.Sprintf("README.md @ %s\n     4\tNEEDLE shout\n     5\tregex42 alpha\n     6\tregex99 beta\n[truncated; next_offset=6]", fixture.second[:12])
	if got != want {
		t.Fatalf("gitRead(paged) = %q, want %q", got, want)
	}

	got, err = gitRead(ctx, repository, "", "README.md", 100, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitRead(past end) error: %v", err)
	}
	want = fmt.Sprintf("README.md @ %s\n(no lines at offset 100)", fixture.second[:12])
	if got != want {
		t.Fatalf("gitRead(past end) = %q, want %q", got, want)
	}

	got, err = gitRead(ctx, repository, fixture.first, "README.md", 0, 200, gitMaxResultBytes)
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

	if _, err := gitRead(ctx, repository, "", "assets/logo.bin", 0, 200, gitMaxResultBytes); err == nil || !strings.Contains(err.Error(), "git: file is binary") {
		t.Fatalf("gitRead(binary) error = %v, want binary rejection", err)
	}
}

func TestGitSearch(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()
	short := fixture.second[:12]

	got, err := gitSearch(ctx, repository, "", "needle", "", false, false, 0, 20, gitMaxResultBytes)
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

	got, err = gitSearch(ctx, repository, "", "needle", "", false, true, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(case sensitive) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\nREADME.md:2:needle here", short)
	if got != want {
		t.Fatalf("gitSearch(case sensitive) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "regex[0-9]+", "", true, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(regex) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\nREADME.md:5:regex42 alpha\nREADME.md:6:regex99 beta", short)
	if got != want {
		t.Fatalf("gitSearch(regex) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "regex[0-9]+", "", false, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(literal regex string) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\n(no matches)", short)
	if got != want {
		t.Fatalf("gitSearch(literal regex string) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "package", "src", false, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(scoped) error: %v", err)
	}
	want = fmt.Sprintf("src @ %s\nsrc/main.go:1:package main\nsrc/util.go:1:package main", short)
	if got != want {
		t.Fatalf("gitSearch(scoped) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "nonexistenttoken", "", false, false, 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(no match) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\n(no matches)", short)
	if got != want {
		t.Fatalf("gitSearch(no match) = %q, want %q", got, want)
	}

	got, err = gitSearch(ctx, repository, "", "needle", "", false, false, 0, 2, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitSearch(paged) error: %v", err)
	}
	want = fmt.Sprintf(". @ %s\nREADME.md:2:needle here\nREADME.md:3:Needle capitalized\n[truncated; next_offset=2]", short)
	if got != want {
		t.Fatalf("gitSearch(paged) = %q, want %q", got, want)
	}
}

func TestGitLog(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()

	got, err := gitLog(ctx, repository, "", "", 0, 20, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitLog() error: %v", err)
	}
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("gitLog() = %q, want two commit lines", got)
	}
	if !strings.HasPrefix(lines[0], fixture.second[:12]+"  ") || !strings.Contains(lines[0], "second commit changes readme") {
		t.Fatalf("gitLog() first line = %q, want newest commit first", lines[0])
	}
	if !strings.HasPrefix(lines[1], fixture.first[:12]+"  ") || !strings.Contains(lines[1], "first commit adding files") {
		t.Fatalf("gitLog() second line = %q, want oldest commit", lines[1])
	}
	if !strings.Contains(lines[0], "  Tester  ") {
		t.Fatalf("gitLog() first line = %q, want author field", lines[0])
	}

	got, err = gitLog(ctx, repository, "", "src/main.go", 0, 20, gitMaxResultBytes)
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

	got, err = gitLog(ctx, repository, "", "", 0, 1, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitLog(paged) error: %v", err)
	}
	if !strings.HasPrefix(got, fixture.second[:12]+"  ") {
		t.Fatalf("gitLog(paged) = %q, want newest commit", got)
	}
	if !strings.HasSuffix(got, "[truncated; next_offset=1]") {
		t.Fatalf("gitLog(paged) = %q, want truncation marker", got)
	}
}

func TestGitDiff(t *testing.T) {
	fixture := newGitFixture(t)
	ctx := context.Background()
	repository := fixture.repository()

	got, err := gitDiff(ctx, repository, fixture.first, fixture.second, "", 0, 200, gitMaxResultBytes)
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

	got, err = gitDiff(ctx, repository, fixture.first, "", "", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitDiff(default target) error: %v", err)
	}
	if !strings.Contains(got, "diff --git a/README.md b/README.md") || !strings.Contains(got, "+alpha text changed") {
		t.Fatalf("gitDiff(default target) = %q, want default to HEAD", got)
	}

	got, err = gitDiff(ctx, repository, fixture.first, fixture.second, "src", 0, 200, gitMaxResultBytes)
	if err != nil {
		t.Fatalf("gitDiff(scoped) error: %v", err)
	}
	if got != "(no differences)" {
		t.Fatalf("gitDiff(scoped) = %q, want (no differences)", got)
	}

	got, err = gitDiff(ctx, repository, fixture.first, fixture.second, "", 0, 4, gitMaxResultBytes)
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
}

func toolNames(definitions []tools.Definition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}
