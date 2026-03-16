package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"unicode"
)

func commandFromString(ctx context.Context, dir string, command string) (*exec.Cmd, error) {
	argv, err := parseCommandString(command)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	return cmd, nil
}

func parseCommandString(command string) ([]string, error) {
	input := strings.TrimSpace(command)
	if input == "" {
		return nil, fmt.Errorf("command is empty")
	}

	args := make([]string, 0, 4)
	var current strings.Builder
	var inSingle bool
	var inDouble bool
	argStarted := false

	flush := func() {
		if !argStarted {
			return
		}
		args = append(args, current.String())
		current.Reset()
		argStarted = false
	}

	for _, r := range input {
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
				argStarted = true
				continue
			}
			current.WriteRune(r)
			argStarted = true
		case inDouble:
			if r == '"' {
				inDouble = false
				argStarted = true
				continue
			}
			current.WriteRune(r)
			argStarted = true
		default:
			switch {
			case unicode.IsSpace(r):
				flush()
			case r == '\'':
				inSingle = true
				argStarted = true
			case r == '"':
				inDouble = true
				argStarted = true
			default:
				current.WriteRune(r)
				argStarted = true
			}
		}
	}

	if inSingle || inDouble {
		return nil, fmt.Errorf("command has unterminated quote")
	}
	flush()
	if len(args) == 0 {
		return nil, fmt.Errorf("command is empty")
	}
	if strings.TrimSpace(args[0]) == "" {
		return nil, fmt.Errorf("command argv[0] is empty")
	}
	return args, nil
}
