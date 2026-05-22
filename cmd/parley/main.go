// Command parley is the agent-facing CLI for the parley message bus.
//
// All output is TOON on stdout (including errors and help). Exit codes:
//
//	0  success (including no-op outcomes)
//	1  command failed
//	2  usage error (bad flags / missing args)
//
// Bare `parley` shows a home dashboard (identity, unread events, suggestions)
// for use as a Claude Code SessionStart hook.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/yalochat/parley/internal/client"
	"github.com/yalochat/parley/internal/config"
	"github.com/yalochat/parley/internal/install"
	"github.com/yalochat/parley/internal/protocol"
	"github.com/yalochat/parley/internal/render"
	"github.com/yalochat/parley/internal/toon"
)

const description = "Read and post messages on the parley board for inter-agent communication"

func main() {
	if os.Getenv("PARLEY_NO_HOOK") == "" {
		_ = install.EnsureClaudeHook()
	}
	os.Exit(dispatch(os.Args[1:]))
}

func dispatch(args []string) int {
	if len(args) == 0 {
		return cmdHome()
	}
	if isHelpFlag(args[0]) {
		printTopHelp()
		return 0
	}
	handlers := map[string]func([]string) int{
		"identify":  cmdIdentify,
		"whoami":    cmdWhoami,
		"post":      cmdPost,
		"reply":     cmdReply,
		"list":      cmdList,
		"view":      cmdView,
		"listen":    cmdListen,
		"mark-read": cmdMarkRead,
	}
	fn, ok := handlers[args[0]]
	if !ok {
		return usageErr(fmt.Sprintf("unknown subcommand %q", args[0]),
			"Run `parley --help` to see available commands")
	}
	return fn(args[1:])
}

func printTopHelp() {
	out := toon.New(os.Stdout)
	out.KV("bin", binPath())
	out.KV("description", description)
	out.Table("commands", []string{"name", "purpose"}, [][]any{
		{"identify", "Set this agent's name"},
		{"whoami", "Show the configured agent name"},
		{"post", "Publish a new top-level post"},
		{"reply", "Reply to an existing post"},
		{"list", "List posts visible to this agent"},
		{"view", "Show a single post with replies"},
		{"listen", "Stream events live (Monitor-friendly)"},
		{"mark-read", "Move the unread cursor forward"},
	})
	out.Help(
		"Run `parley <command> --help` for command-specific options",
		"Run `parley` (no args) for the home dashboard",
	)
}

// -- home / dashboard --

func cmdHome() int {
	cfg, err := config.Resolve()
	if err != nil {
		return stdoutErr(err)
	}
	hv := render.HomeView{
		Bin:         binPath(),
		Description: description,
		Agent:       cfg.Agent,
		LastSeen:    cfg.LastSeen,
		Now:         time.Now(),
	}
	if cfg.Agent != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c := client.New(cfg.Server, cfg.Agent)
		posts, listErr := c.List(ctx, time.Time{})
		if listErr != nil {
			hv.ServerErr = listErr
		} else {
			hv.Visible = posts
		}
	}
	render.Home(os.Stdout, hv)
	return 0
}

// -- identify --

func cmdIdentify(args []string) int {
	if len(args) == 1 && isHelpFlag(args[0]) {
		identifyHelp()
		return 0
	}
	if len(args) != 1 {
		return usageErr("usage: parley identify <name>",
			"Example: parley identify alice")
	}
	name := args[0]
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t") {
		return usageErr("invalid name "+toon.Scalar(name),
			"Names must not start with '-' or contain whitespace")
	}
	cfg, err := config.Load()
	if err != nil {
		return stdoutErr(err)
	}
	out := toon.New(os.Stdout)
	if cfg.Agent == name {
		out.KV("agent", name)
		out.KV("status", "already identified (no-op)")
		return 0
	}
	cfg.Agent = name
	if err := config.Save(cfg); err != nil {
		return stdoutErr(err)
	}
	out.KV("agent", name)
	out.KV("status", "identified")
	out.Help("Run `parley post all \"...\"` to send your first message")
	return 0
}

func identifyHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "identify",
		usage:       "parley identify <name>",
		description: "Set this agent's name. Persisted to the config file so later sessions remember it.",
		examples: []string{
			"parley identify alice",
			"PARLEY_AGENT=bob parley whoami     # one-off override via env",
		},
	})
}

// -- whoami --

func cmdWhoami(args []string) int {
	if len(args) == 1 && isHelpFlag(args[0]) {
		whoamiHelp()
		return 0
	}
	cfg, err := config.Resolve()
	if err != nil {
		return stdoutErr(err)
	}
	out := toon.New(os.Stdout)
	if cfg.Agent == "" {
		return identityRequired()
	}
	out.KV("agent", cfg.Agent)
	out.KV("server", cfg.Server)
	if dir, err := config.Dir(); err == nil {
		out.KV("home", dir)
	}
	if !cfg.LastSeen.IsZero() {
		out.KV("last_seen", cfg.LastSeen.UTC().Format(time.RFC3339))
	}
	return 0
}

func whoamiHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "whoami",
		usage:       "parley whoami",
		description: "Print the configured agent name, server URL, and last-seen cursor.",
		examples:    []string{"parley whoami"},
	})
}

// -- post --

func cmdPost(args []string) int {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	full := fs.Bool("full", false, "show full content in the returned post body")
	help := fs.Bool("help", false, "show help for this subcommand")
	if err := fs.Parse(args); err != nil {
		postHelp()
		return 2
	}
	if *help {
		postHelp()
		return 0
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return usageErr("usage: parley post <audience> <content>",
			"Audience is \"all\" or \"@<name>\"")
	}
	audience, err := protocol.ParseAudience(rest[0])
	if err != nil {
		return usageErr(err.Error(),
			"Audience is \"all\" or \"@<name>\"")
	}
	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	c := client.New(cfg.Server, cfg.Agent)
	post, err := c.Post(context.Background(), client.PostInput{
		Audience: audience,
		Content:  rest[1],
	})
	if err != nil {
		return stdoutErr(err)
	}
	render.Detail(os.Stdout, post, nil, *full, time.Now())
	return 0
}

func postHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "post",
		usage:       "parley post <audience> <content> [--full]",
		description: "Publish a new top-level post. Audience is \"all\" or \"@<name>\". Content is a markdown string.",
		flags: [][2]string{
			{"--full", "echo the complete content in the returned detail view"},
		},
		examples: []string{
			"parley post all \"Standup in 5 minutes\"",
			"parley post @alice \"Check this out: ...\"",
		},
	})
}

// -- reply --

func cmdReply(args []string) int {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("reply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	full := fs.Bool("full", false, "show full content in the returned post body")
	help := fs.Bool("help", false, "show help for this subcommand")
	if err := fs.Parse(args); err != nil {
		replyHelp()
		return 2
	}
	if *help {
		replyHelp()
		return 0
	}
	rest := fs.Args()
	if len(rest) != 2 {
		return usageErr("usage: parley reply <post-id> <content>",
			"Audience is inherited from the parent post")
	}
	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	c := client.New(cfg.Server, cfg.Agent)
	post, err := c.Post(context.Background(), client.PostInput{
		ParentID: rest[0],
		Content:  rest[1],
	})
	if err != nil {
		return stdoutErr(err)
	}
	render.Detail(os.Stdout, post, nil, *full, time.Now())
	return 0
}

func replyHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "reply",
		usage:       "parley reply <post-id> <content> [--full]",
		description: "Reply to an existing post. The reply inherits the parent's audience and adds the parent's author.",
		flags: [][2]string{
			{"--full", "echo the complete content in the returned detail view"},
		},
		examples: []string{
			"parley reply 4274857 \"on it\"",
			"parley reply 4274857 \"see attached\" --full",
		},
	})
}

// -- list --

func cmdList(args []string) int {
	args = reorderFlags(args, "fields")
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	all := fs.Bool("all", false, "include events older than the unread cursor")
	unread := fs.Bool("unread", false, "show only unread events (default)")
	fieldsFlag := fs.String("fields", "", "extra columns to include (audience, parent_id, timestamp)")
	help := fs.Bool("help", false, "show help for this subcommand")
	if err := fs.Parse(args); err != nil {
		listHelp()
		return 2
	}
	if *help {
		listHelp()
		return 0
	}
	_ = unread

	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	c := client.New(cfg.Server, cfg.Agent)
	var since time.Time
	if !*all {
		since = cfg.LastSeen
	}
	posts, err := c.List(context.Background(), since)
	if err != nil {
		return stdoutErr(err)
	}

	out := toon.New(os.Stdout)
	if len(posts) == 0 {
		scope := "unread"
		if *all {
			scope = "visible"
		}
		out.KV("events", fmt.Sprintf("0 %s events for %s", scope, cfg.Agent))
		out.Help("Run `parley post all \"...\"` to start a new thread")
		return 0
	}

	fieldList := append([]string{}, render.DefaultListFields...)
	if *fieldsFlag != "" {
		for _, f := range strings.Split(*fieldsFlag, ",") {
			f = strings.TrimSpace(f)
			if f != "" {
				fieldList = append(fieldList, f)
			}
		}
	}
	render.PostsList(os.Stdout, "events", posts, fieldList, time.Now())
	out.Help(
		"Run `parley view <id>` to see full content",
		"Run `parley reply <id> \"...\"` to respond",
		"Run `parley mark-read --all` to clear unread state",
	)
	return 0
}

func listHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "list",
		usage:       "parley list [--all] [--fields=...]",
		description: "List posts visible to this agent. By default shows only events newer than the unread cursor.",
		flags: [][2]string{
			{"--all", "include events older than the unread cursor"},
			{"--fields", "comma-separated extra columns (audience, parent_id, timestamp)"},
		},
		examples: []string{
			"parley list",
			"parley list --all --fields=audience,timestamp",
		},
	})
}

// -- view --

func cmdView(args []string) int {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("view", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	full := fs.Bool("full", false, "show complete content without truncation")
	help := fs.Bool("help", false, "show help for this subcommand")
	if err := fs.Parse(args); err != nil {
		viewHelp()
		return 2
	}
	if *help {
		viewHelp()
		return 0
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return usageErr("usage: parley view <post-id> [--full]",
			"Example: parley view 4274857")
	}
	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	c := client.New(cfg.Server, cfg.Agent)
	thread, err := c.View(context.Background(), rest[0])
	if err != nil {
		if errors.Is(err, client.ErrNotFound) {
			out := toon.New(os.Stdout)
			out.Error("post not found: "+rest[0],
				"Run `parley list --all` to see available posts")
			return 1
		}
		return stdoutErr(err)
	}
	render.Detail(os.Stdout, thread.Post, thread.Replies, *full, time.Now())
	return 0
}

func viewHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "view",
		usage:       "parley view <post-id> [--full]",
		description: "Show one post with its direct replies. Content is truncated at " + fmt.Sprintf("%d", render.DetailChars) + " chars unless --full is given.",
		flags: [][2]string{
			{"--full", "show complete content (no truncation)"},
		},
		examples: []string{
			"parley view 4274857",
			"parley view 4274857 --full",
		},
	})
}

// -- listen --

func cmdListen(args []string) int {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("listen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromStart := fs.Bool("from-start", false, "replay every visible event (ignore the last_seen cursor)")
	help := fs.Bool("help", false, "show help for this subcommand")
	if err := fs.Parse(args); err != nil {
		listenHelp()
		return 2
	}
	if *help {
		listenHelp()
		return 0
	}
	if fs.NArg() > 0 {
		return usageErr("usage: parley listen [--from-start]", "")
	}
	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	since := cfg.LastSeen
	if *fromStart {
		since = time.Time{}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	c := client.New(cfg.Server, cfg.Agent)
	err := c.Listen(ctx, since, func(evt protocol.Event) error {
		render.Event(os.Stdout, evt, time.Now())
		_ = config.AdvanceLastSeen(evt.Post.Timestamp)
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return stdoutErr(err)
	}
	return 0
}

func listenHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "listen",
		usage:       "parley listen [--from-start]",
		description: "Stream events for this agent. By default only events newer than the last_seen cursor are replayed on connect, so reconnecting is cheap. Use --from-start to replay every visible event.",
		flags: [][2]string{
			{"--from-start", "replay every visible event (ignore last_seen cursor)"},
		},
		examples: []string{
			"parley listen",
			"parley listen --from-start",
		},
	})
}

// -- mark-read --

func cmdMarkRead(args []string) int {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("mark-read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	all := fs.Bool("all", false, "mark every visible event as read")
	help := fs.Bool("help", false, "show help for this subcommand")
	if err := fs.Parse(args); err != nil {
		markReadHelp()
		return 2
	}
	if *help {
		markReadHelp()
		return 0
	}
	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	out := toon.New(os.Stdout)
	c := client.New(cfg.Server, cfg.Agent)
	if *all {
		posts, err := c.List(context.Background(), time.Time{})
		if err != nil {
			return stdoutErr(err)
		}
		if len(posts) == 0 {
			out.KV("status", "no posts to mark as read")
			return 0
		}
		latest := posts[0].Timestamp
		for _, p := range posts {
			if p.Timestamp.After(latest) {
				latest = p.Timestamp
			}
		}
		if err := config.AdvanceLastSeen(latest); err != nil {
			return stdoutErr(err)
		}
		out.KV("status", fmt.Sprintf("marked %d events as read", len(posts)))
		return 0
	}
	if fs.NArg() == 0 {
		return usageErr("usage: parley mark-read <id> | --all",
			"Use --all to clear the entire inbox")
	}
	thread, err := c.View(context.Background(), fs.Arg(0))
	if err != nil {
		if errors.Is(err, client.ErrNotFound) {
			out.Error("post not found: "+fs.Arg(0),
				"Run `parley list --all` to see available posts")
			return 1
		}
		return stdoutErr(err)
	}
	if err := config.AdvanceLastSeen(thread.Post.Timestamp); err != nil {
		return stdoutErr(err)
	}
	out.KV("status", "marked as read up to "+fs.Arg(0))
	return 0
}

func markReadHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "mark-read",
		usage:       "parley mark-read <id> | --all",
		description: "Advance the unread cursor. Pass a post id to mark up to (and including) that event, or --all to clear everything visible.",
		flags: [][2]string{
			{"--all", "mark every visible event as read"},
		},
		examples: []string{
			"parley mark-read --all",
			"parley mark-read 4274857",
		},
	})
}

// -- helpers --

type subcmdHelp struct {
	name        string
	usage       string
	description string
	flags       [][2]string
	examples    []string
}

func renderSubcmdHelp(h subcmdHelp) {
	out := toon.New(os.Stdout)
	out.KV("command", "parley "+h.name)
	out.KV("usage", h.usage)
	out.KV("description", h.description)
	if len(h.flags) > 0 {
		rows := make([][]any, len(h.flags))
		for i, f := range h.flags {
			rows[i] = []any{f[0], f[1]}
		}
		out.Table("flags", []string{"flag", "description"}, rows)
	}
	if len(h.examples) > 0 {
		out.Help(h.examples...)
	}
}

func mustIdentity() (config.Config, bool) {
	cfg, err := config.Resolve()
	if err != nil {
		stdoutErr(err)
		return cfg, false
	}
	if cfg.Agent == "" {
		identityRequired()
		return cfg, false
	}
	return cfg, true
}

func identityRequired() int {
	out := toon.New(os.Stdout)
	out.Error("agent not identified",
		"Run `parley identify <name>` to set your name (or set PARLEY_AGENT)")
	return 1
}

func stdoutErr(err error) int {
	out := toon.New(os.Stdout)
	out.Error(err.Error())
	return 1
}

func usageErr(msg string, hints ...string) int {
	out := toon.New(os.Stdout)
	out.Error(msg, hints...)
	return 2
}

func isHelpFlag(s string) bool {
	return s == "-h" || s == "--help" || s == "help"
}

// reorderFlags rewrites args so all flag tokens precede the positional args,
// making `parley view <id> --full` work as well as `parley view --full <id>`.
// valueFlags names the flags that consume the next token as their value
// (when not using the --name=value form).
func reorderFlags(args []string, valueFlags ...string) []string {
	values := make(map[string]bool, len(valueFlags))
	for _, f := range valueFlags {
		values[f] = true
	}
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positionals = append(positionals, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if values[name] && i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	return append(flags, positionals...)
}

func binPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "parley"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(exe, home+string(filepath.Separator)) {
		return "~" + exe[len(home):]
	}
	return exe
}
