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
	// consolidateConfigParameters walks env, finds every entry whose key
	// is GIT_CONFIG_PARAMETERS, merges their values into a single
	// space-separated token list, appends colorOverride (idempotently),
	// and rewrites env so the consolidated entry sits at the first
	// GIT_CONFIG_PARAMETERS slot with the duplicates dropped. This
	// matters because git's parser concatenates duplicate keys with a
	// NUL byte (and on some libcs rejects the format outright), so
	// leaving two GIT_CONFIG_PARAMETERS entries in env would re-break
	// Diff / DiffHead under ambient color.
	env := consolidateConfigParameters(base, colorOverride)
	env = append(env,
		"GIT_EDITOR=true",
		"GIT_SEQUENCE_EDITOR=true",
		"GIT_TERMINAL_PROMPT=0",
		// Read-only commands such as status and rev-parse must not refresh the
		// index as a side effect. Mutating commands still take required locks.
		"GIT_OPTIONAL_LOCKS=0",
	)
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

// consolidateConfigParameters rewrites env so that exactly one
// GIT_CONFIG_PARAMETERS entry exists. When the caller passes base with
// no GIT_CONFIG_PARAMETERS at all, the consolidated entry is appended
// with token as the only value. When one or more entries are already
// present, their values are joined with single spaces (git's token
// separator), token is appended if not already present (idempotent),
// the result lives at the first GIT_CONFIG_PARAMETERS slot, and every
// subsequent duplicate is dropped.
//
// git parses GIT_CONFIG_PARAMETERS as a single space-separated token
// list. Leaving multiple entries in env causes libgit2-style code
// paths to concatenate them with NUL bytes, which surfaces as
// "bogus format in GIT_CONFIG_PARAMETERS" or a silently ignored second
// entry, both of which re-introduce the ANSI leak this helper exists
// to prevent.
func consolidateConfigParameters(env []string, token string) []string {
	firstIdx := -1
	var tokens []string
	// Pass 1: locate the first GIT_CONFIG_PARAMETERS slot and collect
	// every existing token (across all duplicate entries) into one
	// ordered list. Token order is preserved so callers that depend
	// on a particular color.ui appearing later than http.proxy (for
	// last-wins override) keep that ordering.
	for i, kv := range env {
		if !strings.HasPrefix(kv, "GIT_CONFIG_PARAMETERS=") {
			continue
		}
		if firstIdx == -1 {
			firstIdx = i
		}
		tokens = append(tokens, strings.Fields(strings.TrimPrefix(kv, "GIT_CONFIG_PARAMETERS="))...)
	}
	// Append token idempotently.
	for _, t := range tokens {
		if t == token {
			token = ""
			break
		}
	}
	if token != "" {
		tokens = append(tokens, token)
	}
	consolidated := "GIT_CONFIG_PARAMETERS=" + strings.Join(tokens, " ")
	if firstIdx == -1 {
		return append(env, consolidated)
	}
	// Replace the first GCP entry; drop every subsequent GCP entry.
	out := make([]string, 0, len(env))
	out = append(out, env[:firstIdx]...)
	out = append(out, consolidated)
	for i := firstIdx + 1; i < len(env); i++ {
		if strings.HasPrefix(env[i], "GIT_CONFIG_PARAMETERS=") {
			continue
		}
		out = append(out, env[i])
	}
	return out
}
