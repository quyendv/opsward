package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/quyendv/opsward/internal/app"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/identity"
)

//nolint:gosec // G101: usage text, not a credential
const tokenUsage = `usage: opsward token <action> --config FILE ...

actions:
  create --subject S [--group G]... [--ttl 90d] [--description D]
                     create an API token and print it once
  list               list API tokens, without their secrets
  revoke ID          revoke an API token, named by its ID or the whole token
`

// stringList collects a repeated flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func token(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError(tokenUsage)
	}
	action := args[0]
	fs := flag.NewFlagSet("token "+action, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", "", "configuration file")
	subject := fs.String("subject", "", "")
	description := fs.String("description", "", "")
	ttl := fs.String("ttl", "90d", "")
	var groups stringList
	fs.Var(&groups, "group", "")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	switch action {
	case "create", "list", "revoke":
	default:
		return usageError(tokenUsage)
	}
	if action == "revoke" && fs.NArg() != 1 {
		return errors.New("usage: opsward token revoke --config FILE ID")
	}
	if action == "create" && *subject == "" {
		return errors.New("--subject is required")
	}
	if *path == "" {
		return errors.New("--config is required")
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	if cfg.Identity.Mode != config.ModeRemote {
		return errors.New("API tokens are used only in remote mode (identity.mode: remote)")
	}
	adm, err := app.OpenAdmin(ctx, cfg, app.Options{Stderr: stderr, Stdout: stdout})
	if err != nil {
		return err
	}
	defer func() { _ = adm.Close() }()

	switch action {
	case "create":
		d, err := config.ParseDuration(*ttl)
		if err != nil {
			return fmt.Errorf("--ttl: %w", err)
		}
		tok, t, err := adm.CreateToken(ctx, actor(), identity.TokenRequest{Subject: *subject, Groups: groups, Description: *description, TTL: d})
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "created token %s for %s, expires %s; it is shown once:\n", t.ID, t.Subject, t.ExpiresAt.Format(time.RFC3339))
		fmt.Fprintln(stdout, tok)
	case "list":
		return listTokens(ctx, adm, stdout)
	case "revoke":
		if err := adm.RevokeToken(ctx, actor(), fs.Arg(0)); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "revoked token %s\n", fs.Arg(0))
	}
	return nil
}

func listTokens(ctx context.Context, adm *app.Admin, stdout io.Writer) error {
	tokens, err := adm.Store.ListAPITokens(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSUBJECT\tGROUPS\tSTATUS\tEXPIRES\tLAST USED\tDESCRIPTION")
	for _, t := range tokens {
		status := "active"
		switch {
		case !t.RevokedAt.IsZero():
			status = "revoked"
		case !now.Before(t.ExpiresAt):
			status = "expired"
		}
		used := "never"
		if !t.LastUsedAt.IsZero() {
			used = t.LastUsedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Subject, strings.Join(t.Groups, ","), status,
			t.ExpiresAt.Format(time.RFC3339), used, t.Description)
	}
	return tw.Flush()
}

// actor names the person running an administrative command in the audit record.
func actor() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "os:" + u.Username
	}
	if u := os.Getenv("USER"); u != "" {
		return "os:" + u
	}
	return "os:unknown"
}

// usageError prints usage and exits 2.
type usageError string

func (u usageError) Error() string { return string(u) }
