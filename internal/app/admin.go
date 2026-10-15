package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/quyendv/opsward/internal/audit"
	"github.com/quyendv/opsward/internal/config"
	"github.com/quyendv/opsward/internal/core"
	"github.com/quyendv/opsward/internal/identity"
	"github.com/quyendv/opsward/internal/store"
)

// AdapterCLI identifies administrative actions taken with the opsward command.
const AdapterCLI = "cli"

// adminRule is the decision recorded for administrative actions: whoever can open the
// store may take them, so there is no policy to consult.
const adminRule = "store-access"

// Admin is the store, audit and token management, without tools or providers, for
// administrative commands. docs/design/0005.
type Admin struct {
	Store    store.Store
	Audit    *audit.Writer
	Identity *identity.Authenticator

	maxTTL  time.Duration
	closers []io.Closer
}

// OpenAdmin opens the configured store and audit stream.
func OpenAdmin(ctx context.Context, cfg *config.Config, opt Options) (*Admin, error) {
	// Commands print their results on stdout, a token among them; records there would
	// mix with it.
	if cfg.Audit.Stream.Type == "stdout" {
		c := *cfg
		c.Audit.Stream.Type = "stderr"
		cfg = &c
	}
	st, w, closers, err := openAudit(ctx, cfg, opt)
	if err != nil {
		return nil, err
	}
	return &Admin{
		Store: st, Audit: w, Identity: identity.New(identity.Options{Store: st, Groups: cfg.Identity.Groups, Logger: opt.Logger}),
		maxTTL: cfg.Identity.APITokens.MaxTTL.Std(), closers: closers,
	}, nil
}

// Close releases the store and any stream file.
func (a *Admin) Close() error {
	var errs []error
	for _, c := range slices.Backward(a.closers) {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// CreateToken creates an API token for req, as actor. The intent is audited before the
// token is stored; the secret is never audited.
func (a *Admin) CreateToken(ctx context.Context, actor string, req identity.TokenRequest) (string, store.APIToken, error) {
	req.MaxTTL = a.maxTTL
	token, t, err := a.Identity.NewToken(req)
	if err != nil {
		return "", store.APIToken{}, err
	}
	args := map[string]any{
		"id": t.ID, "subject": t.Subject, "groups": anySlice(t.Groups),
		"description": t.Description, "expiresAt": t.ExpiresAt.Format(time.RFC3339),
	}
	stored := false
	err = a.audited(ctx, actor, "create_api_token", args, func() error {
		err := a.Store.CreateAPIToken(ctx, t)
		stored = err == nil
		return err
	})
	if err != nil {
		if stored {
			// The secret is gone, so the token is unusable but valid: say which to revoke.
			return "", store.APIToken{}, fmt.Errorf("%w; token %s was stored, revoke it with: opsward token revoke %s", err, t.ID, t.ID)
		}
		return "", store.APIToken{}, err
	}
	return token, t, nil
}

// RevokeToken revokes the token that arg names, as actor: its ID, or the whole token,
// which is never recorded.
func (a *Admin) RevokeToken(ctx context.Context, actor, arg string) error {
	id, err := identity.TokenID(arg)
	if err != nil {
		return err
	}
	return a.audited(ctx, actor, "revoke_api_token", map[string]any{"id": id}, func() error {
		ok, err := a.Store.RevokeAPIToken(ctx, id, time.Now().UTC())
		if err == nil && !ok {
			err = fmt.Errorf("no API token %q", id)
		}
		return err
	})
}

// audited writes an intent record, runs fn only if that succeeded, and writes its
// outcome, as the engine does for destructive tools.
func (a *Admin) audited(ctx context.Context, actor, action string, args map[string]any, fn func() error) error {
	rec := audit.Record{
		Phase:     audit.PhaseIntent,
		Principal: core.Principal{Subject: actor, Adapter: AdapterCLI},
		Tool:      action, Risk: core.RiskDestructive, Args: args,
		Decision: &core.Decision{Effect: core.EffectAllow, Rule: adminRule},
	}
	if _, err := a.Audit.Write(ctx, rec); err != nil {
		return fmt.Errorf("%s not attempted, audit failed: %w", action, err)
	}
	start := time.Now()
	runErr := fn()
	rec.Phase, rec.ID, rec.Time = audit.PhaseOutcome, "", time.Time{}
	rec.Outcome = &audit.Outcome{Status: audit.StatusOK, DurationMs: time.Since(start).Milliseconds()}
	if runErr != nil {
		rec.Outcome.Status, rec.Outcome.Error = audit.StatusError, runErr.Error()
	}
	// The action has happened: an interrupt must not lose its record.
	if _, err := a.Audit.Write(context.WithoutCancel(ctx), rec); err != nil {
		return errors.Join(runErr, fmt.Errorf("%s: outcome not audited: %w", action, err))
	}
	return runErr
}

// anySlice keeps audit arguments in the shape JSON decoding gives them, so that a
// record re-read from the store hashes the same.
func anySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
