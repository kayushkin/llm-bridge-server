package executor

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

type Result struct {
	Status string
	Output string
}

func Run(command string, timeout time.Duration) Result {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	output := buf.String()
	if err != nil {
		return Result{Status: "error", Output: output + err.Error()}
	}
	return Result{Status: "success", Output: output}
}
