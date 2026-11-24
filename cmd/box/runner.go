package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

// Cmd is one external command (incus or git).
type Cmd struct {
	Name   string
	Args   []string
	Env    []string  // added to the inherited environment
	Dir    string    // working directory; "" is the current one
	Stdin  io.Reader // optional input for the command
	Stdout io.Writer // when set, stdout streams here instead of into Result.Stdout
	Stderr io.Writer // when set, stderr streams here instead of into Result.Stderr
}

// Result is what a finished command left behind.
type Result struct {
	Stdout, Stderr string
	Code           int
}

// Runner runs external commands. A nonzero exit is a Result, not an error;
// the error is for commands that could not start or were cancelled.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Stdin = c.Stdin
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if c.Stderr != nil {
		cmd.Stderr = c.Stderr
	}
	err := cmd.Run()
	res := Result{Stdout: out.String(), Stderr: errb.String()}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.Code = ee.ExitCode()
		return res, nil
	}
	return res, err
}
