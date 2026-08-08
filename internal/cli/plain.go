package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/agent0ai/spynel/internal/app"
	"github.com/agent0ai/spynel/internal/config"
	"github.com/agent0ai/spynel/internal/core"
	"github.com/agent0ai/spynel/internal/history"
)

const (
	// Keep even worst-case JSON escaping below history's four-MiB per-record
	// reader bound. Larger inputs belong in repeatable --attach files.
	maxCLIInputBytes         = 512 * 1024
	maxCLIConversationTail   = 1000
	maxCLIConversationRunes  = 2 * 1024 * 1024
	defaultConversationLimit = 100
)

type messageRunOptions struct {
	JSON         bool
	Stream       bool
	FollowupOnly bool
	Attachments  []string
	Output       io.Writer
}

type stringListFlag []string

func (values *stringListFlag) String() string { return strings.Join(*values, ",") }

func (values *stringListFlag) Set(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("attachment path cannot be empty")
	}
	*values = append(*values, value)
	return nil
}

func runSendCommand(name string, args []string, version string, followupOnly bool) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	configPath := flags.String("config", "", "path to spynel.yaml")
	conversation := flags.String("conversation", "local", "durable CLI conversation name")
	stream := flags.Bool("stream", false, "print response deltas as they arrive")
	jsonOutput := flags.Bool("json", false, "emit response events as NDJSON")
	stdin := flags.Bool("stdin", false, "read the message body from standard input")
	var attachments stringListFlag
	flags.Var(&attachments, "attach", "copy a file into Spynel and attach it (repeatable)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *stream && *jsonOutput {
		return errors.New("--stream and --json are mutually exclusive; JSON output already streams every event")
	}
	if strings.TrimSpace(*conversation) == "" {
		return fmt.Errorf("%s conversation name cannot be empty", name)
	}
	text := ""
	var err error
	if *stdin || len(flags.Args()) > 0 || len(attachments) == 0 {
		text, err = cliMessageText(flags.Args(), *stdin, os.Stdin)
	}
	if err != nil {
		return fmt.Errorf("usage: spynel %s [--config PATH] [--conversation NAME] [--stream|--json] [--stdin] <text>: %w", name, err)
	}
	return runMessageMode(*configPath, *conversation, text, version, messageRunOptions{
		JSON: *jsonOutput, Stream: *stream, FollowupOnly: followupOnly,
		Attachments: append([]string(nil), attachments...), Output: os.Stdout,
	})
}

func runNotifyCommand(args []string, version string) error {
	flags := flag.NewFlagSet("notify", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to spynel.yaml")
	origin := flags.String("origin", "", "stable channel/conversation origin")
	stdin := flags.Bool("stdin", false, "read the notification from standard input")
	if err := flags.Parse(args); err != nil {
		return err
	}
	text, err := cliMessageText(flags.Args(), *stdin, os.Stdin)
	if err != nil {
		return fmt.Errorf("usage: spynel notify [--config PATH] --origin CHANNEL/CONVERSATION [--stdin] <message>: %w", err)
	}
	if strings.TrimSpace(*origin) == "" {
		return errors.New("--origin is required")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var id string
	if client, active, clientErr := activeWorkspaceClient(ctx, cfg); clientErr != nil {
		return clientErr
	} else if active {
		id, err = client.Notify(ctx, *origin, text)
	} else {
		service, buildErr := buildService(cfg, version)
		if buildErr != nil {
			return buildErr
		}
		defer service.Close()
		id, err = service.Notify(ctx, *origin, text)
	}
	if err != nil {
		return err
	}
	fmt.Println("queued notification " + id)
	return nil
}

func cliMessageText(arguments []string, stdin bool, input io.Reader) (string, error) {
	if stdin && len(arguments) > 0 {
		return "", errors.New("message text and --stdin cannot be used together")
	}
	if stdin {
		data, err := io.ReadAll(io.LimitReader(input, maxCLIInputBytes+1))
		if err != nil {
			return "", err
		}
		if len(data) > maxCLIInputBytes {
			return "", fmt.Errorf("standard input exceeds %d bytes", maxCLIInputBytes)
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			return "", errors.New("standard input is empty")
		}
		return text, nil
	}
	text := strings.TrimSpace(strings.Join(arguments, " "))
	if text == "" {
		return "", errors.New("message text is required")
	}
	return text, nil
}

func runFrameworkCLICommand(command string, args []string, version string) error {
	name := command
	if name == "" {
		name = "command"
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	configPath := flags.String("config", "", "path to spynel.yaml")
	conversation := flags.String("conversation", "local", "durable CLI conversation name")
	jsonOutput := flags.Bool("json", false, "emit response events as NDJSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*conversation) == "" {
		return errors.New("command conversation name cannot be empty")
	}
	arguments := flags.Args()
	if command == "" {
		if len(arguments) == 0 {
			return errors.New("usage: spynel command [--config PATH] [--conversation NAME] [--json] <name> [arguments]")
		}
		command = arguments[0]
		arguments = arguments[1:]
	}
	command = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(command), "/"))
	for _, visual := range []string{"theme", "title", "welcome", "resume", "primary", "quit", "exit"} {
		if command == visual {
			return fmt.Errorf("/%s is interactive-TUI-specific and is not exposed by the plain CLI", command)
		}
	}
	text := "/" + command
	if len(arguments) > 0 {
		text += " " + strings.Join(arguments, " ")
	}
	return runFrameworkMessageMode(*configPath, *conversation, text, version, messageRunOptions{JSON: *jsonOutput, Output: os.Stdout})
}

// runFrameworkMessageMode routes shared slash commands through the owner when
// present, but deliberately does not start a coding harness for an offline
// one-shot command. Configuration, histories, logs, tasks, and extensions are
// application behavior and must remain usable before a harness is installed.
func runFrameworkMessageMode(configPath, conversation, text, version string, options messageRunOptions) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if client, active, clientErr := activeWorkspaceClient(ctx, cfg); clientErr != nil {
		return clientErr
	} else if active {
		return runMessageWithOutput(ctx, client.Handle, conversation, text, options)
	}
	service, err := buildService(cfg, version)
	if err != nil {
		return err
	}
	defer service.Close()
	if err := runMessageWithOutput(ctx, service.Handle, conversation, text, options); err != nil {
		return err
	}
	select {
	case <-service.UpdateRequests():
		return &updateRequest{}
	default:
		return nil
	}
}

func runStatusCLICommand(args []string, version string, output io.Writer) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to spynel.yaml")
	conversation := flags.String("conversation", "local", "durable CLI conversation name")
	jsonOutput := flags.Bool("json", false, "emit structured JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(*conversation) == "" {
		return errors.New("usage: spynel status [--config PATH] [--conversation NAME] [--json]")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var status app.StatusSnapshot
	if client, active, clientErr := activeWorkspaceClient(ctx, cfg); clientErr != nil {
		return clientErr
	} else if active {
		status, err = client.Status(ctx, *conversation)
	} else {
		service, buildErr := buildService(cfg, version)
		if buildErr != nil {
			return buildErr
		}
		defer service.Close()
		status, err = service.Status(core.Message{Channel: "cli", Conversation: *conversation, Sender: "cli"})
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(output).Encode(status)
	}
	_, err = fmt.Fprintln(output, app.FormatStatus(status))
	return err
}

type conversationListRecord struct {
	Channel      string    `json:"channel"`
	Conversation string    `json:"conversation"`
	UpdatedAt    time.Time `json:"updated_at"`
	LastRole     string    `json:"last_role,omitempty"`
	Preview      string    `json:"preview,omitempty"`
	Path         string    `json:"path"`
}

type conversationTail struct {
	Channel      string          `json:"channel"`
	Conversation string          `json:"conversation"`
	Path         string          `json:"path"`
	Entries      []history.Entry `json:"entries"`
}

type conversationBranch struct {
	SourceChannel      string `json:"source_channel"`
	SourceConversation string `json:"source_conversation"`
	Channel            string `json:"channel"`
	Conversation       string `json:"conversation"`
	Path               string `json:"path"`
}

func runConversationCommand(args []string, output io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: spynel conversations <list|show|resume> [options]")
	}
	switch strings.ToLower(args[0]) {
	case "list", "ls":
		return listCLIConversations(args[1:], output)
	case "show", "get":
		return showCLIConversation(args[1:], output)
	case "resume", "branch":
		return resumeCLIConversation(args[1:], output)
	default:
		return errors.New("usage: spynel conversations <list|show|resume> [options]")
	}
}

func listCLIConversations(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("conversations list", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to spynel.yaml")
	limit := flags.Int("limit", defaultConversationLimit, "maximum conversations")
	jsonOutput := flags.Bool("json", false, "emit a JSON array")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *limit < 1 || *limit > 1000 {
		return errors.New("usage: spynel conversations list [--config PATH] [--limit 1..1000] [--json]")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	conversations, err := history.New(cfg.StatePath("history")).List(*limit)
	if err != nil {
		return err
	}
	records := make([]conversationListRecord, 0, len(conversations))
	for _, conversation := range conversations {
		records = append(records, conversationListRecord{
			Channel: conversation.Channel, Conversation: conversation.Conversation,
			UpdatedAt: conversation.UpdatedAt, LastRole: conversation.LastRole,
			Preview: conversation.Preview, Path: conversation.Path,
		})
	}
	if *jsonOutput {
		return json.NewEncoder(output).Encode(records)
	}
	if len(records) == 0 {
		_, err = fmt.Fprintln(output, "No saved conversations.")
		return err
	}
	if _, err := fmt.Fprintln(output, "CHANNEL\tCONVERSATION\tUPDATED\tLAST\tPREVIEW"); err != nil {
		return err
	}
	for _, record := range records {
		preview := strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(record.Preview)
		if _, err := fmt.Fprintf(output, "%s\t%s\t%s\t%s\t%s\n", record.Channel, record.Conversation, record.UpdatedAt.Format(time.RFC3339), record.LastRole, preview); err != nil {
			return err
		}
	}
	return nil
}

func showCLIConversation(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("conversations show", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to spynel.yaml")
	tail := flags.Int("tail", 50, "maximum newest entries")
	characters := flags.Int("chars", 500000, "maximum formatted characters")
	jsonOutput := flags.Bool("json", false, "emit structured JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 || *tail < 1 || *tail > maxCLIConversationTail || *characters < 1 || *characters > maxCLIConversationRunes {
		return fmt.Errorf("usage: spynel conversations show [--config PATH] [--tail 1..%d] [--chars 1..%d] [--json] <channel> <conversation>", maxCLIConversationTail, maxCLIConversationRunes)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	channelName, conversation := flags.Arg(0), flags.Arg(1)
	store := history.New(cfg.StatePath("history"))
	entries, path, err := store.RecentEntries(channelName, conversation, *tail, *characters)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("conversation %s/%s does not exist", channelName, conversation)
		}
		return err
	}
	if *jsonOutput {
		return json.NewEncoder(output).Encode(conversationTail{Channel: channelName, Conversation: conversation, Path: path, Entries: entries})
	}
	if _, err := fmt.Fprintln(output, "History: "+path); err != nil {
		return err
	}
	for _, entry := range entries {
		label := entry.Role
		if entry.Sender != "" {
			label += " (" + entry.Sender + ")"
		}
		if _, err := fmt.Fprintf(output, "[%s] %s: %s\n", entry.At.Format(time.RFC3339), label, entry.Content); err != nil {
			return err
		}
	}
	return nil
}

func resumeCLIConversation(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("conversations resume", flag.ContinueOnError)
	configPath := flags.String("config", "", "path to spynel.yaml")
	jsonOutput := flags.Bool("json", false, "emit structured JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: spynel conversations resume [--config PATH] [--json] <channel> <conversation>")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	sourceChannel, sourceConversation := flags.Arg(0), flags.Arg(1)
	conversation, path, err := history.New(cfg.StatePath("history")).BranchTo(sourceChannel, sourceConversation, "cli")
	if err != nil {
		return err
	}
	branch := conversationBranch{
		SourceChannel: sourceChannel, SourceConversation: sourceConversation,
		Channel: "cli", Conversation: conversation, Path: path,
	}
	if *jsonOutput {
		return json.NewEncoder(output).Encode(branch)
	}
	_, err = fmt.Fprintln(output, conversation)
	return err
}
