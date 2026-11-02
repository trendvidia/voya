package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/trendvidia/voya/internal/auth"
	"github.com/trendvidia/voya/internal/config"
	configv1 "github.com/trendvidia/voya/proto/voya/config/v1"
)

// tokenCmd is `voya token` (#210): a signed JWT for a persona, minted from
// the config and the signing key alone — no daemon, because signing needs
// the key and not the listeners, and minting a token before `voya run` is
// the normal order of events on a fresh terminal.
//
// The token is the payload and the only thing on stdout, no trailing prose,
// so `$(voya token --as has-write-scope)` is a token; everything the command
// says about itself goes to stderr like the rest of voya (README —
// Configuring voya).
//
// Personas come from config (ADR 006: fixtures describing cases), so
// `missing scope` and `wrong audience` are the no-scopes and wrong-audience
// personas and no flag exists for either. -expired and -bad-signature are
// flags because they have no persona field to live in: expiry and the
// signature are what minting itself stamps. One defect at a time — a token
// broken two ways cannot show which rejection fired, which is the whole
// point of a deliberately invalid token.
func tokenCmd(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("voya token", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	as := fs.String("as", "", "the persona to mint for, by name from auth.personas")
	expired := fs.Bool("expired", false, "mint the token already expired, everything else valid")
	badSig := fs.Bool("bad-signature", false, "corrupt the signature after signing, everything else valid")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			tokenUsage(stdout)
			return nil
		}
		tokenUsage(stderr)
		return err
	}
	if fs.NArg() != 0 {
		tokenUsage(stderr)
		return fmt.Errorf("token takes no arguments, got %q — the persona is named with --as", strings.Join(fs.Args(), " "))
	}
	if *expired && *badSig {
		return errors.New("token mints one deliberate defect at a time: --expired or --bad-signature, not both — a token broken two ways cannot show which rejection fired")
	}
	defect := auth.Valid
	switch {
	case *expired:
		defect = auth.Expired
	case *badSig:
		defect = auth.BadSignature
	}

	cfg, err := config.Load(context.Background(), config.Dir())
	if err != nil {
		return err
	}
	persona, err := personaNamed(cfg.GetAuth().GetPersonas(), *as)
	if err != nil {
		return err
	}
	key, err := auth.LoadOrGenerate(config.Dir())
	if err != nil {
		return err
	}
	token, err := auth.Mint(key, cfg.GetAuth(), persona, time.Now(), defect)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, token)
	return nil
}

// personaNamed finds name among the configured personas. Both failure modes
// — no --as at all, and a name nothing answers to — end in the same list of
// the personas that do exist, because the config is the only place the
// answer is and this is the error a developer hits most.
func personaNamed(personas []*configv1.Persona, name string) (*configv1.Persona, error) {
	names := make([]string, len(personas))
	for i, p := range personas {
		if p.GetName() == name {
			return p, nil
		}
		names[i] = p.GetName()
	}
	available := "the configuration has none — add auth.personas to voya.pxf"
	if len(names) > 0 {
		available = strings.Join(names, ", ")
	}
	if name == "" {
		return nil, fmt.Errorf("token needs --as <persona>; the configured personas are: %s", available)
	}
	return nil, fmt.Errorf("no persona named %q; the configured personas are: %s", name, available)
}

func tokenUsage(w io.Writer) {
	fmt.Fprint(w, `Mint a signed JWT for a persona, valid or deliberately not (ADR 006).

Usage:
  voya token --as <persona> [--expired | --bad-signature]

The personas come from auth.personas in the configuration; --as with a name
that is not there lists the ones that are. --expired mints the token an hour
past its own lifetime, --bad-signature corrupts the signature after signing;
each leaves everything else about the token valid, and the wrong-audience
and no-scopes cases are personas, not flags. The token is the only thing on
stdout. No daemon is needed.
`)
}
