package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/latebit-io/demarkus/client/fetch"
	"github.com/latebit-io/demarkus/client/links"
	"github.com/latebit-io/demarkus/client/memory/audit"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/guidance"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry/catalog"
)

// cmdDoctor audits a store and prints the hygiene report. Exit 1 only when
// the audit cannot be trusted; findings exit 0.
func cmdDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	scope := fs.String("scope", "/", "audit root: / or /<slug>/")
	storeID := fs.String("store", "", "catalog id of the store to audit (default: the project's bound store)")
	dir := fs.String("dir", "", "project directory for the binding (default: harness project variable)")
	deep := fs.Bool("deep", false, "also check metadata lost across versions")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		fail("doctor: " + err.Error())
	}
	if len(fs.Args()) != 0 {
		fail("doctor: usage: doctor [--scope /<slug>/] [--store ID] [--dir DIR] [--deep] [--json]")
	}
	die := func(err error) { fail("doctor: " + err.Error()) }
	// bound is the store the session's slash commands write to; "" when the
	// binding cannot be read, which only matters under --store.
	id, bound := *storeID, ""
	r, err := guidance.ResolveProject(*dir)
	switch {
	case err == nil && r.State != project.StateStale:
		bound = r.Store
	case id == "" && err != nil:
		die(err)
	case id == "":
		die(errors.New("bound store '" + r.Store + "' is " + guidance.Hint(r)))
	}
	if id == "" {
		id = bound
	}
	client, store, err := resolveStore(id)
	if err != nil {
		die(err)
	}
	defer client.Close() // no error to handle; on die the process exits and the OS closes the sockets
	report, err := audit.Run(context.Background(), store, audit.Options{Scope: *scope, Deep: *deep})
	if err != nil {
		die(err)
	}
	if id == config.LocalMemoryID && report.Scope == "/" {
		addTokenDrift(report)
	}
	out := report.Markdown()
	if id == bound {
		report.Commands = soulCommands()
		out = report.Markdown()
	} else {
		out = otherStoreNote(id, bound) + "\n\n" + out
	}
	if *asJSON {
		if out, err = report.JSON(); err != nil {
			die(err)
		}
	}
	if _, err := fmt.Fprintln(os.Stdout, out); err != nil {
		die(fmt.Errorf("write output: %w", err))
	}
}

// soulCommands words a fix as the memory plugin's slash command, which is
// what a person reading this report can run.
func soulCommands() audit.Commands {
	return audit.Commands{
		Curate:  func(p string) string { return "/soul-curate " + p },
		Archive: func(p string) string { return "/soul-archive " + p },
		Promote: func(p string) string { return "/promote " + p },
		Batch:   func(scope string) string { return "/soul-curate " + scope },
	}
}

// otherStoreNote heads a report of a store the session is not bound to: the
// slash commands write to the bound store, so its fixes name actions alone.
func otherStoreNote(audited, bound string) string {
	binding := "this session has no usable binding"
	if bound != "" {
		binding = "this session is bound to `" + bound + "`"
	}
	return "> Store `" + audited + "` was audited, and " + binding + ". The fixes below name actions, not commands: " +
		"/soul-curate, /soul-archive and /promote write to the bound store. Rebind with /soul-default to run them here."
}

// addTokenDrift folds the local write-auth probe into the report: drift is a
// finding, an unverifiable state a coverage gap, healthy nothing.
func addTokenDrift(report *audit.Report) {
	verdict, err := provision.VerifyAuth()
	switch {
	case err != nil:
		report.Coverage = append(report.Coverage, "write auth not verified: "+err.Error())
	case strings.HasPrefix(verdict, "write auth healthy"):
	case strings.HasPrefix(verdict, "token drift"):
		report.Findings = append(report.Findings, audit.Finding{Check: audit.CheckTokenDrift, Path: "provision verify-auth", Detail: verdict, Fix: "writes fail unauthorized until the token or registry is fixed"})
	default:
		report.Coverage = append(report.Coverage, "write auth not verified: "+verdict)
	}
}

// resolveStore maps a catalog id to a client. Broker stores need the
// harness's OAuth session and are refused.
func resolveStore(id string) (*fetch.Client, *audit.ClientStore, error) {
	ep, err := catalog.Resolve(id)
	if err != nil {
		return nil, nil, err
	}
	if ep.Broker {
		return nil, nil, errors.New("store '" + id + "' is a broker store; the doctor audits local and direct-QUIC stores only")
	}
	hostPort, err := links.DialHost(ep.Host)
	if err != nil {
		return nil, nil, fmt.Errorf("store '%s' host: %w", id, err)
	}
	client := fetch.NewClient(fetch.Options{Insecure: ep.Insecure, DialTimeout: 3 * time.Second, RequestTimeout: 15 * time.Second})
	return client, &audit.ClientStore{Client: client, Host: hostPort, Token: ep.Token}, nil
}
