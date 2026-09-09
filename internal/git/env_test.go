package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// resolveEnv collapses a KEY=VALUE slice into a map using last-wins semantics,
// matching how exec resolves duplicate keys.
func resolveEnv(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		m[k] = v
	}
	return m
}

func TestNonInteractiveEnv_SetsGitOverrides(t *testing.T) {
	// Use an explicit base so the assertion is independent of any
	// ambient GIT_CONFIG_PARAMETERS that the host shell may have set
	// (CI presets and agent harnesses routinely inject color.ui=always).
	got := resolveEnv(NonInteractiveEnvFrom([]string{}, ""))

	want := map[string]string{
		"GIT_EDITOR":          "true",
		"GIT_SEQUENCE_EDITOR": "true",
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_OPTIONAL_LOCKS":  "0",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
	if got["GIT_CONFIG_PARAMETERS"] != "'color.ui=never'" {
		t.Errorf("env GIT_CONFIG_PARAMETERS = %q, want \"'color.ui=never'\"", got["GIT_CONFIG_PARAMETERS"])
	}
}

// TestNonInteractiveEnv_OverridesAmbientColorGuard locks in that an ambient
// GIT_CONFIG_PARAMETERS='color.ui=always' (set by some agent harnesses and CI
// presets) cannot leak ANSI escape sequences through Diff / DiffHead / Log.
// The override must be appended to the existing token list using git's
// space-separated key=value format so other parameters (such as
// http.proxy=...) are preserved verbatim.
func TestNonInteractiveEnv_OverridesAmbientColorGuard(t *testing.T) {
	got := resolveEnv(NonInteractiveEnvFrom([]string{
		"GIT_CONFIG_PARAMETERS='color.ui=always'",
	}, ""))

	value := got["GIT_CONFIG_PARAMETERS"]
	if value == "" {
		t.Fatalf("GIT_CONFIG_PARAMETERS missing; full env = %v", got)
	}
	// Both the ambient color.ui=always and the override color.ui=never
	// must be present, with the override last so git's last-wins parser
	// picks it.
	if !strings.HasSuffix(value, "'color.ui=never'") {
		t.Errorf("GIT_CONFIG_PARAMETERS = %q, want it to end with \"'color.ui=never'\"", value)
	}
	if !strings.Contains(value, "'color.ui=always'") {
		t.Errorf("GIT_CONFIG_PARAMETERS = %q, must preserve ambient \"'color.ui=always'\"", value)
	}
}

// TestNonInteractiveEnv_PreservesOtherConfigParameters locks in that
// appending the color.ui=never override does NOT clobber other entries in
// GIT_CONFIG_PARAMETERS. Some agent harnesses inject http.proxy and other
// settings via this variable, and silently dropping them would break
// offline / proxied git operation.
func TestNonInteractiveEnv_PreservesOtherConfigParameters(t *testing.T) {
	got := resolveEnv(NonInteractiveEnvFrom([]string{
		"GIT_CONFIG_PARAMETERS=http.proxy=http://proxy.example.com:8080 'color.ui=always'",
	}, ""))

	value := got["GIT_CONFIG_PARAMETERS"]
	if !strings.Contains(value, "http.proxy=http://proxy.example.com:8080") {
		t.Errorf("GIT_CONFIG_PARAMETERS = %q, want http.proxy preserved", value)
	}
	if !strings.Contains(value, "'color.ui=always'") {
		t.Errorf("GIT_CONFIG_PARAMETERS = %q, want \"'color.ui=always'\" preserved", value)
	}
	if !strings.HasSuffix(value, "'color.ui=never'") {
		t.Errorf("GIT_CONFIG_PARAMETERS = %q, want \"'color.ui=never'\" appended", value)
	}
}

// TestNonInteractiveEnv_DisablesColorEndToEnd is a black-box guard against
// regressions in the env-layer color override. With GIT_CONFIG_PARAMETERS
// pre-set to force color on (the upstream harness behavior), Diff must still
// return a plain-text stream that includes the literal "+" prefix instead of
// the colorized "\x1b[32m+\x1b[m\x1b[32m..." sequence git emits when color
// is on. This is the test that would have caught the original bug.
func TestNonInteractiveEnv_DisablesColorEndToEnd(t *testing.T) {
	dir := initTestRepo(t)
	ctx := context.Background()

	base := run(t, dir, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(dir, "color-guard.txt"), "plain line\n")
	run(t, dir, "git", "add", ".")
	run(t, dir, "git", "commit", "-m", "add color-guard")

	// Force git to want color on, exactly like the upstream agent harness
	// does. The helper must still override it.
	t.Setenv("GIT_CONFIG_PARAMETERS", "'color.ui=always'")

	diff, err := Diff(ctx, dir, base, run(t, dir, "git", "rev-parse", "HEAD"))
	if err != nil {
		t.Fatalf("Diff under forced color failed: %v", err)
	}
	if strings.Contains(diff, "\x1b[") {
		t.Fatalf("diff still contains ANSI escapes under forced color: %q", diff)
	}
	if !strings.Contains(diff, "+plain line") {
		t.Fatalf("diff must contain plain \"+plain line\" marker, got: %q", diff)
	}
}

func TestNonInteractiveEnv_OverridesAmbientEditor(t *testing.T) {
	t.Setenv("GIT_EDITOR", "vim")
	t.Setenv("GIT_SEQUENCE_EDITOR", "nano")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")

	got := resolveEnv(NonInteractiveEnv(""))

	if got["GIT_EDITOR"] != "true" {
		t.Errorf("GIT_EDITOR = %q, want \"true\" (ambient vim must be overridden)", got["GIT_EDITOR"])
	}
	if got["GIT_SEQUENCE_EDITOR"] != "true" {
		t.Errorf("GIT_SEQUENCE_EDITOR = %q, want \"true\"", got["GIT_SEQUENCE_EDITOR"])
	}
	if got["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want \"0\"", got["GIT_TERMINAL_PROMPT"])
	}
}

func TestNonInteractiveEnv_PreservesAmbientEnv(t *testing.T) {
	t.Setenv("NM_ENV_PROBE_XYZ", "kept")

	got := resolveEnv(NonInteractiveEnv(""))

	if got["NM_ENV_PROBE_XYZ"] != "kept" {
		t.Errorf("ambient env not preserved: NM_ENV_PROBE_XYZ = %q, want \"kept\"", got["NM_ENV_PROBE_XYZ"])
	}
}

func TestNonInteractiveEnvFrom_UsesBaseAndOverridesGitKeys(t *testing.T) {
	got := resolveEnv(NonInteractiveEnvFrom([]string{
		"PATH=/custom/bin",
		"NM_ENV_PROBE_XYZ=kept",
		"GIT_TERMINAL_PROMPT=1",
	}, ""))

	if got["PATH"] != "/custom/bin" {
		t.Errorf("PATH = %q, want custom base PATH", got["PATH"])
	}
	if got["NM_ENV_PROBE_XYZ"] != "kept" {
		t.Errorf("NM_ENV_PROBE_XYZ = %q, want kept", got["NM_ENV_PROBE_XYZ"])
	}
	if got["GIT_TERMINAL_PROMPT"] != "0" {
		t.Errorf("GIT_TERMINAL_PROMPT = %q, want noninteractive override", got["GIT_TERMINAL_PROMPT"])
	}
}

// TestNonInteractiveEnv_SetsPWDToDir locks in the PWD coupling. Assigning
// cmd.Env disables os/exec's automatic PWD=Cmd.Dir injection, so the helper
// must restore it; otherwise os.Getwd in the child resolves symlinks (e.g.
// /tmp -> /private/tmp on macOS) and reports a different working directory.
func TestNonInteractiveEnv_SetsPWDToDir(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")

	got := resolveEnv(NonInteractiveEnv("/work/dir"))

	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		if got["PWD"] != "/somewhere/else" {
			t.Errorf("PWD = %q, want ambient PWD on %s", got["PWD"], runtime.GOOS)
		}
		return
	}

	if got["PWD"] != "/work/dir" {
		t.Errorf("PWD = %q, want \"/work/dir\"", got["PWD"])
	}
}

// TestNonInteractiveEnv_AbsolutizesRelativeDir locks in that a relative dir is
// absolutized before injection, matching os/exec (go.dev/issue/50599). POSIX
// defines PWD as an absolute pathname; a relative value like "." propagates
// through git receive-pack into hooks, where macOS /bin/sh (bash 3.2) trusts
// it and `pwd` collapses to "." (issue #269).
func TestNonInteractiveEnv_AbsolutizesRelativeDir(t *testing.T) {
	t.Setenv("PWD", "/somewhere/else")

	got := resolveEnv(NonInteractiveEnv("."))

	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		if got["PWD"] != "/somewhere/else" {
			t.Errorf("PWD = %q, want ambient PWD on %s", got["PWD"], runtime.GOOS)
		}
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if got["PWD"] != wd {
		t.Errorf("PWD = %q, want absolute %q for relative dir \".\"", got["PWD"], wd)
	}
}

func TestNonInteractiveEnv_EmptyDirLeavesAmbientPWD(t *testing.T) {
	t.Setenv("PWD", "/ambient/pwd")

	got := resolveEnv(NonInteractiveEnv(""))

	if got["PWD"] != "/ambient/pwd" {
		t.Errorf("PWD = %q, want ambient \"/ambient/pwd\" when dir is empty", got["PWD"])
	}
}
