package git

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

type environmentContextKey struct{}

// WithEnvironment freezes an environment overlay into ctx for Git commands
// and any hooks or credential helpers they spawn.
func WithEnvironment(ctx context.Context, overlay runenv.Overlay) context.Context {
	if overlay.Empty() {
		return ctx
	}
	return context.WithValue(ctx, environmentContextKey{}, overlay.Clone())
}

func nonInteractiveEnvForContext(ctx context.Context, dir string) []string {
	base := os.Environ()
	if ctx != nil {
		if overlay, ok := ctx.Value(environmentContextKey{}).(runenv.Overlay); ok {
			base = overlay.Apply(base)
		}
	}
	return NonInteractiveEnvFrom(base, dir)
}

// NonInteractiveEnv returns the environment for a subprocess that may invoke
// git, with git forced into a fully non-interactive mode. It is intended for
// cmd.Env on any subprocess that may run git (our own git calls and the coding
// agents we spawn).
//
// Without these overrides, git operations such as `git rebase --continue` or
// `git commit` open $EDITOR to confirm a commit message, and remote operations
// can block on a credential prompt. In a headless agent subprocess there is no
// TTY, so the editor or prompt hangs until the agent times out. Pointing the
// editors at `true` makes git accept the existing message immediately, and
// GIT_TERMINAL_PROMPT=0 fails fast instead of blocking on credentials. The
// overrides are appended last so they win over any ambient values (exec
// resolves duplicate keys using the last occurrence).
//
// Pass the same directory assigned to cmd.Dir (or "" when it is unset). When
// cmd.Env is left nil, os/exec injects PWD=cmd.Dir automatically; assigning
// cmd.Env disables that, so callers must thread the working directory through
// here to preserve symlinked working-directory paths (for example /tmp vs
// /private/tmp on macOS, which os.Getwd reports differently depending on PWD).
func NonInteractiveEnv(dir string) []string {
	return NonInteractiveEnvFrom(os.Environ(), dir)
}

// NonInteractiveEnvFrom is NonInteractiveEnv applied to an explicit base
// environment. A nil base means the current process environment.
func NonInteractiveEnvFrom(base []string, dir string) []string {
	if base == nil {
		base = os.Environ()
	}

	// Force `git diff` / `git log` output to never carry ANSI escapes.
	// Tooling (some agent harnesses and CI presets) injects
	// GIT_CONFIG_PARAMETERS='color.ui=always' into the agent's
	// environment, which leaks ESC[1m...ESC[m tokens into every byte
	// stream we capture for downstream parsing (notably
	// internal/git.Diff and DiffHead), causing callers that look for
	// plain "+added" markers to fail.
	//
	// We must NOT simply append a second GIT_CONFIG_PARAMETERS entry:
	// when the same env key appears more than once in cmd.Env, git's
	// getenv-based parser concatenates them with a literal NUL byte
	// (it ignores the second one entirely on most libcs, and on others
	// it sees "bogus format in GIT_CONFIG_PARAMETERS"). The correct
	// override is to append a single token to the existing value using
	// git's documented space-separated key=value format. If no value is
	// present, we set a minimal one.
	colorOverride := "'color.ui=never'"
	hasExistingGCP := false
	for _, kv := range base {
		if strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS=") {
			hasExistingGCP = true
			break
		}
	}
	if hasExistingGCP {
		base = appendConfigParameters(base, colorOverride)
	}

	env := append(append([]string(nil), base...),
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_TERMINAL_PROMPT=0",
		// Read-only commands such as status and rev-parse must not refresh the
		// index as a side effect. Mutating commands still take required locks.
		"GIT_OPTIONAL_LOCKS=0",
	)
	if !hasExistingGCP {
		env = append(env, "GIT_CONFIG_PARAMETERS="+colorOverride)
	}
	// Mirror os/exec, which only injects PWD when Cmd.Env is nil, skips it on
	// these platforms, and absolutizes Cmd.Dir first (go.dev/issue/50599):
	// POSIX defines PWD as "an absolute pathname of the current working
	// directory". Injecting a relative dir verbatim (for example ".") poisons
	// every descendant that trusts PWD — macOS /bin/sh is bash 3.2, whose pwd
	// builtin reports "." when PWD="." leaks through git receive-pack into a
	// hook, which is how the post-receive hook of issue #269 ended up passing
	// `--gate .`.
	if dir != "" && runtime.GOOS != "windows" && runtime.GOOS != "plan9" {
		if abs, err := filepath.Abs(dir); err == nil {
			env = append(env, "PWD="+abs)
		}
	}
	return env
}

// appendConfigParameters appends a single "key=value" token to the
// GIT_CONFIG_PARAMETERS entry already present in env. git parses
// GIT_CONFIG_PARAMETERS as space-separated key=value tokens, so the
// existing http.proxy=... and other parameters are preserved verbatim
// while color.ui=never wins because it appears later.
func appendConfigParameters(env []string, token string) []string {
	for i, kv := range env {
		if !strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS=") {
			continue
		}
		current := strings.TrimPrefix(kv, "GIT_CONFIG_PARAMETERS=")
		// Avoid duplicating the override if a caller already added it.
		for _, existing := range strings.Fields(current) {
			if existing == token {
				return env
			}
		}
		env[i] = "GIT_CONFIG_PARAMETERS=" + current + " " + token
		return env
	}
	return env
}
