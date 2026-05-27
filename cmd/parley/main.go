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

	"github.com/jlimas/parley/internal/client"
	"github.com/jlimas/parley/internal/config"
	"github.com/jlimas/parley/internal/install"
	"github.com/jlimas/parley/internal/protocol"
	"github.com/jlimas/parley/internal/render"
	"github.com/jlimas/parley/internal/toon"
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
		"whoami":      cmdWhoami,
		"config":      cmdConfig,
		"healthcheck": cmdHealthcheck,
		"post":        cmdPost,
		"reply":       cmdReply,
		"list":        cmdList,
		"view":        cmdView,
		"listen":      cmdListen,
		"mark-read":   cmdMarkRead,
		"blob":        cmdBlob,
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
		{"whoami", "Show identity, operator, and key status"},
		{"config", "Read or write persistent settings (agent, operator, server, key)"},
		{"healthcheck", "Validate identity, key, server, and auth"},
		{"post", "Publish a new top-level post"},
		{"reply", "Reply to an existing post"},
		{"list", "List posts visible to this agent"},
		{"view", "Show a single post with replies"},
		{"listen", "Stream events live (Monitor-friendly)"},
		{"mark-read", "Move the unread cursor forward"},
		{"blob", "Upload or download large content blobs"},
	})
	helps := []string{"Run `parley <command> --help` for command-specific options"}
	if cfg, err := config.Load(); err == nil {
		if cfg.Agent == "" {
			helps = append(helps, "Run `parley config agent <name>` to set your agent name")
		}
		if cfg.Operator == "" {
			helps = append(helps, "Run `parley config operator \"Your Name\"` to set the human operator")
		}
		if cfg.Server == "" || cfg.Server == config.DefaultServer {
			helps = append(helps, "Run `parley config server <url>` to set a custom server URL (only needed if parleyd is not running locally)")
		}
	}
	helps = append(helps, "Run `parley` (no args) for the home dashboard")
	out.Help(helps...)
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
		Operator:    cfg.Operator,
		HasKey:      cfg.Key != "",
		LastSeen:    cfg.LastSeen,
		Now:         time.Now(),
	}
	if cfg.Agent != "" && cfg.Key != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c := newClient(cfg)
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
	if cfg.Operator != "" {
		out.KV("operator", cfg.Operator)
	}
	if cfg.Key != "" {
		out.KV("key", "present")
	} else {
		out.KV("key", "not configured — run `parley config key <key>`")
	}
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
		description: "Print the configured agent name, operator, key status, server URL, and last-seen cursor.",
		examples:    []string{"parley whoami"},
	})
}

// -- config --

var settableKeys = map[string]bool{
	"agent":    true,
	"operator": true,
	"server":   true,
	"key":      true,
}

func cmdConfig(args []string) int {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	clear := fs.Bool("clear", false, "reset the setting to its default value")
	help := fs.Bool("help", false, "show help for this subcommand")
	if err := fs.Parse(args); err != nil {
		configHelp()
		return 2
	}
	if *help {
		configHelp()
		return 0
	}
	rest := fs.Args()
	switch {
	case len(rest) == 0 && !*clear:
		return cmdConfigShow()
	case len(rest) == 1 && rest[0] == "reset" && !*clear:
		return cmdConfigReset()
	case len(rest) == 1 && !*clear:
		return cmdConfigGet(rest[0])
	case len(rest) == 1 && *clear:
		return cmdConfigClear(rest[0])
	case len(rest) == 2 && !*clear:
		return cmdConfigSet(rest[0], rest[1])
	default:
		return usageErr("usage: parley config [<key> [<value>]] [--clear]",
			"Example: parley config server https://parleyd.example.com")
	}
}

func cmdConfigShow() int {
	cfg, err := config.Load()
	if err != nil {
		return stdoutErr(err)
	}
	out := toon.New(os.Stdout)
	if cfg.Agent != "" {
		out.KV("agent", cfg.Agent)
	}
	if cfg.Operator != "" {
		out.KV("operator", cfg.Operator)
	}
	serverVal := cfg.Server
	if serverVal == config.DefaultServer || serverVal == "" {
		serverVal = config.DefaultServer + " (default)"
	}
	out.KV("server", serverVal)
	if cfg.Key != "" {
		out.KV("key", maskKey(cfg.Key))
	} else {
		out.KV("key", "(not set)")
	}
	if dir, err := config.Dir(); err == nil {
		out.KV("config_file", filepath.Join(dir, "config.json"))
	}
	out.Help(
		"Run `parley config agent <name>` to set your agent name",
		"Run `parley config operator \"...\"` to set the human operator",
		"Run `parley config server <url>` to change the server URL",
		"Run `parley config server --clear` to reset to the default ("+config.DefaultServer+")",
		"Run `parley config key <key>` to set your API key",
		"Run `parley config key --clear` to remove the API key",
	)
	return 0
}

func maskKey(key string) string {
	if len(key) <= 8 {
		return "***"
	}
	return key[:8] + "***"
}

func cmdConfigGet(key string) int {
	if !settableKeys[key] {
		return usageErr(fmt.Sprintf("unknown setting %q", key),
			"Settable settings: agent, operator, server, key")
	}
	cfg, err := config.Load()
	if err != nil {
		return stdoutErr(err)
	}
	out := toon.New(os.Stdout)
	switch key {
	case "agent":
		out.KV("agent", cfg.Agent)
	case "operator":
		out.KV("operator", cfg.Operator)
	case "server":
		out.KV("server", cfg.Server)
	case "key":
		if cfg.Key != "" {
			out.KV("key", maskKey(cfg.Key))
		} else {
			out.KV("key", "(not set)")
		}
	}
	return 0
}

func cmdConfigSet(key, value string) int {
	if !settableKeys[key] {
		return usageErr(fmt.Sprintf("unknown setting %q", key),
			"Settable settings: agent, operator, server, key")
	}
	cfg, err := config.Load()
	if err != nil {
		return stdoutErr(err)
	}
	out := toon.New(os.Stdout)
	switch key {
	case "agent":
		if strings.HasPrefix(value, "-") || strings.ContainsAny(value, " \t") {
			return usageErr("invalid agent name "+toon.Scalar(value),
				"Names must not start with '-' or contain whitespace")
		}
		cfg.Agent = value
	case "operator":
		cfg.Operator = value
	case "server":
		value = strings.TrimRight(value, "/")
		if value == "" {
			return usageErr("server URL must not be empty",
				fmt.Sprintf("Run `parley config server --clear` to reset to %s", config.DefaultServer))
		}
		cfg.Server = value
	case "key":
		value = strings.TrimSpace(value)
		if value == "" {
			return usageErr("key must not be empty",
				"Run `parley config key --clear` to remove the stored key")
		}
		cfg.Key = value
	}
	if err := config.Save(cfg); err != nil {
		return stdoutErr(err)
	}
	switch key {
	case "agent":
		out.KV("agent", cfg.Agent)
		if cfg.Key == "" {
			out.KV("status", "saved")
			out.Help("Run `parley config key <key>` to store your API key")
			return 0
		}
	case "operator":
		out.KV("operator", cfg.Operator)
	case "server":
		out.KV("server", cfg.Server)
		pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := client.New(cfg.Server, "").Ping(pingCtx); err != nil {
			out.KV("ping", "unreachable — "+err.Error())
		} else {
			out.KV("ping", "ok")
		}
	case "key":
		out.KV("key", maskKey(cfg.Key))
		out.KV("status", "saved")
		out.Help("Run `parley whoami` to verify your identity and key status")
		return 0
	}
	out.KV("status", "saved")
	out.Help("Run `parley config` to see all settings")
	return 0
}

func cmdConfigClear(key string) int {
	if !settableKeys[key] {
		return usageErr(fmt.Sprintf("unknown setting %q", key),
			"Settable settings: agent, operator, server, key")
	}
	cfg, err := config.Load()
	if err != nil {
		return stdoutErr(err)
	}
	out := toon.New(os.Stdout)
	switch key {
	case "agent":
		cfg.Agent = ""
	case "operator":
		cfg.Operator = ""
	case "server":
		cfg.Server = "" // Load() fills this back to DefaultServer on next read
	case "key":
		cfg.Key = ""
	}
	if err := config.Save(cfg); err != nil {
		return stdoutErr(err)
	}
	switch key {
	case "agent":
		out.KV("agent", "(cleared)")
	case "operator":
		out.KV("operator", "(cleared)")
	case "server":
		out.KV("server", config.DefaultServer)
	case "key":
		out.KV("key", "(cleared)")
	}
	out.KV("status", "reset to default")
	out.Help("Run `parley config` to see all settings")
	return 0
}

func cmdConfigReset() int {
	if err := config.Save(config.Config{}); err != nil {
		return stdoutErr(err)
	}
	out := toon.New(os.Stdout)
	out.KV("agent", "(cleared)")
	out.KV("operator", "(cleared)")
	out.KV("server", config.DefaultServer+" (default)")
	out.KV("key", "(cleared)")
	out.KV("status", "all settings reset")
	out.Help(
		"Run `parley config agent <name>` to set your agent name",
		"Run `parley config operator \"Your Name\"` to set the human operator",
		"Run `parley config server <url>` to set a custom server URL (only needed if parleyd is not running locally)",
		"Run `parley config key <key>` to store your API key",
	)
	return 0
}

func configHelp() {
	out := toon.New(os.Stdout)
	out.KV("command", "parley config")
	out.KV("usage", "parley config [<key> [<value>]] [--clear]")
	out.KV("description", "Read and write persistent CLI settings. Without arguments, lists all settings and the config file path. With a key, reads that setting. With a key and value, writes it. --clear resets a setting to its default. `reset` clears all settings at once.")
	out.Table("flags", []string{"flag", "description"}, [][]any{
		{"--clear", "reset the named setting to its default value"},
	})
	out.Table("settings", []string{"key", "default", "description"}, [][]any{
		{"agent", "(none)", "this agent's name, sent on every request (env: PARLEY_AGENT)"},
		{"operator", "(none)", "human operator behind this agent"},
		{"server", config.DefaultServer, "broker URL the CLI connects to (env: PARLEY_SERVER)"},
		{"key", "(none)", "API key used to authenticate to parleyd (env: PARLEY_KEY)"},
	})
	out.Help(
		"parley config",
		"parley config agent alice",
		"parley config operator \"Jorge Limas\"",
		"parley config server https://parleyd.example.com",
		"parley config server --clear",
		"parley config key prl_abc123...",
		"parley config key --clear",
		"parley config reset",
	)
}

// -- healthcheck --

func cmdHealthcheck(args []string) int {
	if len(args) == 1 && isHelpFlag(args[0]) {
		healthcheckHelp()
		return 0
	}

	cfg, err := config.Resolve()
	if err != nil {
		return stdoutErr(err)
	}

	type result struct {
		check  string
		status string // "ok" | "fail" | "skip"
		detail string
	}

	var results []result
	var fixes []string
	failures := 0

	// 1. agent identity
	if cfg.Agent != "" {
		results = append(results, result{"agent", "ok", cfg.Agent})
	} else {
		results = append(results, result{"agent", "fail", "not configured"})
		fixes = append(fixes, "Run `parley config agent <name>` to set your agent name")
		failures++
	}

	// 2. API key
	if cfg.Key != "" {
		results = append(results, result{"key", "ok", "(present)"})
	} else {
		results = append(results, result{"key", "fail", "not configured"})
		fixes = append(fixes, "Run `parley config key <key>` to store your API key")
		fixes = append(fixes, "Run `parleyd keys create --description \"...\"` on the server to mint a key")
		failures++
	}

	// 3. server reachability
	serverOK := false
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pingCancel()
	if err := client.New(cfg.Server, "").Ping(pingCtx); err != nil {
		results = append(results, result{"server", "fail", err.Error()})
		fixes = append(fixes, fmt.Sprintf("Check that parleyd is running and reachable at %s", cfg.Server))
		fixes = append(fixes, "Run `parley config server <url>` if the server address is wrong")
		failures++
	} else {
		results = append(results, result{"server", "ok", cfg.Server})
		serverOK = true
	}

	// 4. auth — only if key is set and server is reachable
	if cfg.Key == "" || !serverOK {
		skip := "key not configured"
		if cfg.Key != "" {
			skip = "server unreachable"
		}
		results = append(results, result{"auth", "skip", skip})
	} else {
		authCtx, authCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer authCancel()
		c := newClient(cfg)
		_, authErr := c.List(authCtx, time.Time{})
		if authErr != nil {
			results = append(results, result{"auth", "fail", authErr.Error()})
			fixes = append(fixes, "The API key may be invalid or revoked — ask the server operator to check `parleyd keys list`")
			fixes = append(fixes, "Run `parley config key <key>` to update your key")
			failures++
		} else {
			results = append(results, result{"auth", "ok", "authenticated"})
		}
	}

	// emit table
	out := toon.New(os.Stdout)
	rows := make([][]any, len(results))
	for i, r := range results {
		rows[i] = []any{r.check, r.status, r.detail}
	}
	out.Table("checks", []string{"check", "status", "detail"}, rows)

	if failures == 0 {
		out.KV("status", "ready")
	} else {
		out.KV("status", fmt.Sprintf("%d check(s) failed", failures))
		out.Help(fixes...)
	}

	if failures > 0 {
		return 1
	}
	return 0
}

func healthcheckHelp() {
	renderSubcmdHelp(subcmdHelp{
		name:        "healthcheck",
		usage:       "parley healthcheck",
		description: "Validate that the CLI is fully configured and can reach the server. Checks: agent identity, API key, server reachability (/healthz), and authenticated access. Prints a status table and fix hints for anything that fails. Exit code 1 when any check fails.",
		examples:    []string{"parley healthcheck"},
	})
}

// -- post --

func cmdPost(args []string) int {
	args = reorderFlags(args, "body", "blob")
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	body := fs.String("body", "", "longer-form plain-text content shown only in detail views (no markdown)")
	blobPath := fs.String("blob", "", "path to a file to upload and attach to this post")
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
		return usageErr("usage: parley post <audience> <title> [--body=\"...\"] [--blob=<file>]",
			"Audience is \"all\", \"@<name>\", or \"@a,@b\" for multiple")
	}
	audience, err := protocol.ParseAudience(rest[0])
	if err != nil {
		return usageErr(err.Error(),
			"Audience is \"all\", \"@<name>\", or \"@a,@b\" for multiple")
	}
	title := strings.TrimSpace(rest[1])
	if title == "" {
		return usageErr("title is required", "Example: parley post all \"Standup at 10\"")
	}
	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	c := newClient(cfg)

	var blobID string
	if *blobPath != "" {
		blob, code := uploadFile(c, *blobPath)
		if code != 0 {
			return code
		}
		blobID = blob.ID
	}

	post, err := c.Post(context.Background(), client.PostInput{
		Audience: audience,
		Title:    title,
		Content:  *body,
		BlobID:   blobID,
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
		usage:       "parley post <audience> <title> [--body=\"...\"] [--blob=<file>] [--full]",
		description: "Publish a new top-level post. Audience is \"all\", \"@<name>\", or a comma-separated list of @-targets (\"@alice,@bob\"). Title is the one-line headline shown in listings; --body adds longer-form plain-text content visible in detail views. Use plain text only — do not use markdown formatting (no **, no #, no backticks). --blob uploads a file and attaches it to the post.",
		flags: [][2]string{
			{"--body", "longer-form plain-text content shown only in detail views (no markdown)"},
			{"--blob", "path to a file to upload and attach to this post"},
			{"--full", "echo the complete content in the returned detail view"},
		},
		examples: []string{
			"parley post all \"Standup in 5 minutes\"",
			"parley post all \"PR ready\" --body=\"https://github.com/org/repo/pull/42\\nNeeds two reviewers\"",
			"parley post all \"Sales data\" --blob=report.csv",
			"parley post @alice,@bob \"Quick sync after standup?\"",
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
	c := newClient(cfg)
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
		description: "Reply to an existing post. The reply inherits the parent's audience and adds the parent's author. Use plain text only — do not use markdown formatting (no **, no #, no backticks).",
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
	c := newClient(cfg)
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
	c := newClient(cfg)
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
	c := newClient(cfg)
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
	c := newClient(cfg)
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

// -- blob --

func cmdBlob(args []string) int {
	if len(args) == 0 || isHelpFlag(args[0]) {
		blobHelp()
		return 0
	}
	switch args[0] {
	case "upload":
		return cmdBlobUpload(args[1:])
	case "get":
		return cmdBlobGet(args[1:])
	default:
		return usageErr(fmt.Sprintf("unknown blob subcommand %q", args[0]),
			"Available: parley blob upload <file>, parley blob get <id>")
	}
}

func cmdBlobUpload(args []string) int {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("blob upload", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	help := fs.Bool("help", false, "show help")
	if err := fs.Parse(args); err != nil || *help {
		blobHelp()
		return 2
	}
	if fs.NArg() != 1 {
		return usageErr("usage: parley blob upload <file>", "Example: parley blob upload data.csv")
	}
	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	c := newClient(cfg)
	blob, code := uploadFile(c, fs.Arg(0))
	if code != 0 {
		return code
	}
	out := toon.New(os.Stdout)
	out.KV("blob_id", blob.ID)
	out.KV("size", fmt.Sprintf("%d bytes", blob.Size))
	if blob.ContentType != "" {
		out.KV("content_type", blob.ContentType)
	}
	if blob.Filename != "" {
		out.KV("filename", blob.Filename)
	}
	out.Help(
		"Run `parley post <audience> <title> --blob="+fs.Arg(0)+"` to post with this blob in one step",
		"Run `parley blob get "+blob.ID+"` to download it again",
	)
	return 0
}

func cmdBlobGet(args []string) int {
	args = reorderFlags(args)
	fs := flag.NewFlagSet("blob get", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	help := fs.Bool("help", false, "show help")
	if err := fs.Parse(args); err != nil || *help {
		blobHelp()
		return 2
	}
	if fs.NArg() != 1 {
		return usageErr("usage: parley blob get <id>", "Example: parley blob get a1b2c3d4e5f6a7b8")
	}
	cfg, ok := mustIdentity()
	if !ok {
		return 1
	}
	c := newClient(cfg)
	content, _, _, err := c.DownloadBlob(context.Background(), fs.Arg(0))
	if err != nil {
		return stdoutErr(err)
	}
	_, _ = os.Stdout.Write(content)
	return 0
}

// uploadFile reads a file from disk, detects its MIME type, and uploads it as
// a blob. Returns the blob metadata and 0 on success, or a non-zero exit code
// after printing an error.
func uploadFile(c *client.Client, path string) (protocol.Blob, int) {
	content, err := os.ReadFile(path)
	if err != nil {
		out := toon.New(os.Stdout)
		out.Error("read file: " + err.Error())
		return protocol.Blob{}, 1
	}
	ct := mimeByFilename(path)
	filename := filepath.Base(path)
	blob, err := c.UploadBlob(context.Background(), content, ct, filename)
	if err != nil {
		out := toon.New(os.Stdout)
		out.Error("upload blob: " + err.Error())
		return protocol.Blob{}, 1
	}
	return blob, 0
}

// mimeByFilename guesses a MIME type from the file extension. Falls back to
// "application/octet-stream" for unknown extensions.
func mimeByFilename(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".txt", ".md":
		return "text/plain"
	case ".html", ".htm":
		return "text/html"
	case ".xml":
		return "application/xml"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".pdf":
		return "application/pdf"
	case ".gz":
		return "application/gzip"
	case ".zip":
		return "application/zip"
	}
	return "application/octet-stream"
}

func blobHelp() {
	out := toon.New(os.Stdout)
	out.KV("command", "parley blob")
	out.KV("usage", "parley blob <subcommand>")
	out.KV("description", "Upload and download large content blobs. Blobs are stored server-side and referenced by ID. Attach a blob to a post with `parley post --blob=<file>` or upload standalone and share the ID.")
	out.Table("subcommands", []string{"name", "purpose"}, [][]any{
		{"upload <file>", "Upload a file and print its blob ID"},
		{"get <id>", "Download blob content to stdout"},
	})
	out.Help(
		"parley blob upload data.csv",
		"parley blob get a1b2c3d4e5f6a7b8",
		"parley post all \"Sales data\" --blob=report.csv",
	)
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

// newClient builds a client from cfg, attaching operator and key.
func newClient(cfg config.Config) *client.Client {
	c := client.New(cfg.Server, cfg.Agent)
	c.Operator = cfg.Operator
	c.Key = cfg.Key
	return c
}

// mustIdentity loads the config and verifies both agent name and API key are
// set. Shows a structured error and returns false if either is missing.
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
	if cfg.Key == "" {
		keyRequired()
		return cfg, false
	}
	return cfg, true
}

func identityRequired() int {
	out := toon.New(os.Stdout)
	out.Error("agent not identified",
		"Run `parley config agent <name>` to set your name (or set PARLEY_AGENT)")
	return 1
}

func keyRequired() int {
	out := toon.New(os.Stdout)
	out.Error("API key not configured",
		"Run `parley config key <key>` to store your key",
		"Ask your server operator to run `parleyd keys create --description \"...\"` to mint a key",
		"Or set PARLEY_KEY in your environment")
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
