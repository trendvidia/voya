package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	diagv1 "github.com/trendvidia/voya/proto/voya/diag/v1"
)

// routesCmd is `voya routes` (#152): ADR 005's routing table, authority ->
// service -> upstream, from one ListRoutes — the same dial, deadline and two
// not-a-failure answers as `voya status`, through [unary]. One row per live
// registration, sorted by authority; the human table by default, the
// ListRoutesResponse as pxf under -pxf (ADR 003 puts every rendering of
// registry state under the pxf rule). An empty registry is an answer: one
// line saying so, exit 0 — on stdout, and on stderr under -pxf so a pipe
// reading the document reads the document alone.
func routesCmd(args []string, stdout, stderr io.Writer) error {
	asPXF, done, err := pxfFlag("routes", routesUsage, args, stdout, stderr)
	if done {
		return err
	}
	ctx := context.Background()
	target, err := dialDiag(ctx)
	if err != nil {
		return err
	}
	defer target.close()
	note := prose(asPXF, stdout, stderr)
	res, err := unary(ctx, target, note, target.client.ListRoutes, &diagv1.ListRoutesRequest{})
	if err != nil {
		return err
	}
	// One order for both renderings. The sort is the command's promise, not
	// the daemon's (see [sortedByAuthority]), so the document a pipe reads is
	// in it too — otherwise `voya routes` and `voya routes -pxf` would list
	// the same registry in two orders.
	res.Routes = sortedByAuthority(res.GetRoutes())
	if len(res.GetRoutes()) == 0 {
		fmt.Fprintln(note, "no services registered")
	}
	if asPXF {
		return pxfDocument(stdout, res)
	}
	if len(res.GetRoutes()) == 0 {
		return nil
	}
	renderRoutes(stdout, res)
	return nil
}

// sortedByAuthority is the row order `voya routes` promises, over a copy: the
// daemon answers in *name* order, which is not the same order. A name may
// hold a hyphen (registry.ValidName's rule is Fly's), and '-' sorts below the
// '.' that opens the suffix — so a registry holding `api` and `api-v2`
// answers `api, api-v2` and this table prints `api-v2.internal` first. The
// sort is therefore the command's own promise rather than a restatement of
// the server's, which is also why the table does not lean on the schema
// comment keeping it.
func sortedByAuthority(routes []*diagv1.Route) []*diagv1.Route {
	out := slices.Clone(routes)
	slices.SortStableFunc(out, func(a, b *diagv1.Route) int {
		return strings.Compare(a.GetAuthority(), b.GetAuthority())
	})
	return out
}

// renderRoutes writes the table, sorted by authority — sorted here as well as
// in [routesCmd], so that the rendering holds the promise on its own whatever
// order it is handed.
func renderRoutes(w io.Writer, res *diagv1.ListRoutesResponse) {
	routes := sortedByAuthority(res.GetRoutes())
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AUTHORITY\tSERVICE\tUPSTREAM")
	for _, r := range routes {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.GetAuthority(), r.GetService(), r.GetUpstream())
	}
	_ = tw.Flush() // a bytes.Buffer or a terminal; the write error is stdout's, and main's
}

func routesUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  voya routes [-pxf]

The routing table of the running voya, one row per registered service:
authority -> service -> upstream address, sorted by authority. Says "no
services registered" when the registry is empty.

  -pxf   render the ListRoutesResponse as pxf instead of the table
`)
}
