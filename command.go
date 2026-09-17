package egtest

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// CommandError deliberately excludes arguments and subprocess output: both can
// contain Kubernetes credentials, Secret data, or an Envoy runtime artifact.
type CommandError struct {
	Tool string
	Err  error
}

func (e *CommandError) Error() string { return fmt.Sprintf("egtest: %s failed: %v", e.Tool, e.Err) }
func (e *CommandError) Unwrap() error { return e.Err }

func command(ctx context.Context, executable, tool string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = bytes.NewReader(input)
	raw, err := cmd.Output()
	if err != nil {
		// exec.ExitError retains stderr; strip it even when callers unwrap errors.
		if exit, ok := err.(*exec.ExitError); ok {
			exit.Stderr = nil
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, &CommandError{Tool: tool, Err: err}
	}
	return raw, nil
}
