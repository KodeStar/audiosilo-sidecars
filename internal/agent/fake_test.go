package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// fakeCLIOpts configures a generated fake CLI shell script.
type fakeCLIOpts struct {
	versionLine string // printed for `--version`
	response    string // written verbatim to stdout on a run (claude JSON / codex JSONL)
	lastMsg     string // written to the --output-last-message file if that flag is present
	exit        int    // exit code for a run
	sleepSecs   int    // if > 0, sleep this long before responding (timeout tests)
}

// fakeCLI writes an executable shell script that mimics an agent CLI and captures
// what it was invoked with. Returns (scriptPath, captureDir). On a run the script
// records: stdin.txt (the prompt), argv.txt (joined argv), and anthropic.txt/
// codex.txt/openai.txt (the injected key env vars, empty if unset).
func fakeCLI(t *testing.T, opts fakeCLIOpts) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI uses a POSIX shell script")
	}
	dir := t.TempDir()
	cap := filepath.Join(dir, "capture")
	if err := os.MkdirAll(cap, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cap, "response.out"), []byte(opts.response), 0o644); err != nil {
		t.Fatal(err)
	}
	sleepLine := ""
	if opts.sleepSecs > 0 {
		sleepLine = "sleep " + strconv.Itoa(opts.sleepSecs)
	}
	script := "#!/bin/sh\n" +
		"CAP='" + cap + "'\n" +
		"if [ \"$1\" = \"--version\" ]; then printf '%s\\n' '" + opts.versionLine + "'; exit 0; fi\n" +
		"cat > \"$CAP/stdin.txt\"\n" +
		"printf '%s' \"$*\" > \"$CAP/argv.txt\"\n" +
		"printf '%s' \"$ANTHROPIC_API_KEY\" > \"$CAP/anthropic.txt\"\n" +
		"printf '%s' \"$CODEX_API_KEY\" > \"$CAP/codex.txt\"\n" +
		"printf '%s' \"$OPENAI_API_KEY\" > \"$CAP/openai.txt\"\n" +
		"prev=''\n" +
		"for a in \"$@\"; do\n" +
		"  if [ \"$prev\" = \"--output-last-message\" ]; then printf '%s' '" + opts.lastMsg + "' > \"$a\"; fi\n" +
		"  prev=\"$a\"\n" +
		"done\n" +
		sleepLine + "\n" +
		"cat \"$CAP/response.out\"\n" +
		"exit " + strconv.Itoa(opts.exit) + "\n"
	path := filepath.Join(dir, "fakecli")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, cap
}

// readCapture returns the trimmed content of a capture file, or "" if absent.
func readCapture(t *testing.T, capDir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(capDir, name))
	if err != nil {
		return ""
	}
	return string(b)
}
