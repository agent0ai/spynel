package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/agent0ai/spynel/internal/agentdocs"
)

type docsExitError struct{ code int }

func (e docsExitError) Error() string { return "documentation request failed" }
func (e docsExitError) ExitCode() int { return e.code }

func runDocsCommand(args []string, output io.Writer) error {
	request, err := parseDocsArgs(args)
	if err != nil {
		return err
	}
	document, err := agentdocs.Lookup(request)
	if err != nil {
		return err
	}
	text, err := agentdocs.Render(request)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(output, text); err != nil {
		return err
	}
	if document.Error != nil {
		return docsExitError{code: 2}
	}
	return nil
}

func parseDocsArgs(args []string) (agentdocs.Request, error) {
	request := agentdocs.Request{Page: 1, Format: "text"}
	positional := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		switch {
		case argument == "--format":
			index++
			if index >= len(args) {
				return request, fmt.Errorf("usage: spynel docs [TOPIC|search QUERY] [page NUMBER] [--format text|json]")
			}
			request.Format = args[index]
		case strings.HasPrefix(argument, "--format="):
			request.Format = strings.TrimPrefix(argument, "--format=")
		case argument == "--help" || argument == "-h":
			return request, fmt.Errorf("usage: spynel docs [TOPIC|search QUERY] [page NUMBER] [--format text|json]")
		case strings.HasPrefix(argument, "-"):
			return request, fmt.Errorf("unknown docs option %q; use --format text or --format json", argument)
		default:
			positional = append(positional, argument)
		}
	}
	if len(positional) == 0 {
		return request, nil
	}
	if positional[0] == "page" {
		if len(positional) != 2 {
			return request, fmt.Errorf("usage: spynel docs page NUMBER")
		}
		page, err := positivePage(positional[1])
		if err != nil {
			return request, err
		}
		request.Page = page
		return request, nil
	}
	if positional[0] == "search" {
		positional = positional[1:]
		if len(positional) >= 2 && positional[len(positional)-2] == "page" {
			page, err := positivePage(positional[len(positional)-1])
			if err != nil {
				return request, err
			}
			request.Page = page
			positional = positional[:len(positional)-2]
		}
		request.Search = strings.TrimSpace(strings.Join(positional, " "))
		if request.Search == "" {
			return request, fmt.Errorf("usage: spynel docs search QUERY [page NUMBER]")
		}
		return request, nil
	}
	request.Topic = positional[0]
	if len(positional) == 1 {
		return request, nil
	}
	if len(positional) != 3 || positional[1] != "page" {
		return request, fmt.Errorf("usage: spynel docs TOPIC [page NUMBER]")
	}
	page, err := positivePage(positional[2])
	if err != nil {
		return request, err
	}
	request.Page = page
	return request, nil
}

func positivePage(value string) (int, error) {
	page, err := strconv.Atoi(value)
	if err != nil || page < 1 {
		return 0, fmt.Errorf("invalid docs page %q; page must be a positive integer", value)
	}
	return page, nil
}
