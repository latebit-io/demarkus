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
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/config"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/doctor"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/project"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/provision"
	"github.com/latebit-io/demarkus/tools/demarkus-plugin/internal/registry"
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
	_ = fs.Parse(args) // ExitOnError: Parse never returns
	if len(fs.Args()) != 0 {
		fail("doctor: usage: doctor [--scope /<slug>/] [--store ID] [--dir DIR] [--deep] [--json]")
	}
	die := func(err error) { fail("doctor: " + err.Error()) }
	id := *storeID
	if id == "" {
		r, err := project.Resolve(*dir)
		if err != nil {
			die(err)
		}
		if r.State == project.StateStale {
			die(errors.New("bound store '" + r.Store + "' is " + r.Hint()))
		}
		id = r.Store
	}
	store, err := resolveStore(id)
	if err != nil {
		die(err)
	}
	defer store.Client.Close() // no error to handle; on die the process exits and the OS closes the sockets
	report, err := doctor.Run(context.Background(), store, doctor.Options{Scope: *scope, Deep: *deep})
	if err != nil {
		die(err)
	}
	if id == config.LocalMemoryID && report.Scope == "/" {
		addTokenDrift(report)
	}
	out := report.Markdown()
	if *asJSON {
		if out, err = report.JSON(); err != nil {
			die(err)
		}
	}
	if _, err := fmt.Fprintln(os.Stdout, out); err != nil {
		die(fmt.Errorf("write output: %w", err))
	}
}

// addTokenDrift folds the local write-auth probe into the report: drift is a
// finding, an unverifiable state a coverage gap, healthy nothing.
func addTokenDrift(report *doctor.Report) {
	verdict, err := provision.VerifyAuth()
	switch {
	case err != nil:
		report.Coverage = append(report.Coverage, "write auth not verified: "+err.Error())
	case strings.HasPrefix(verdict, "write auth healthy"):
	case strings.HasPrefix(verdict, "token drift"):
		report.Findings = append(report.Findings, doctor.Finding{Check: doctor.CheckTokenDrift, Path: "provision verify-auth", Detail: verdict, Fix: "writes fail unauthorized until the token or registry is fixed"})
	default:
		report.Coverage = append(report.Coverage, "write auth not verified: "+verdict)
	}
}

// resolveStore maps a catalog id to a client. Broker stores need the
// harness's OAuth session and are refused.
func resolveStore(id string) (*doctor.ClientStore, error) {
	ep, err := registry.MemoryEndpoint(id)
	if err != nil {
		return nil, err
	}
	if ep.Broker {
		return nil, errors.New("store '" + id + "' is a broker store; the doctor audits local and direct-QUIC stores only")
	}
	hostPort, _, err := fetch.ParseMarkURL(ep.Host)
	if err != nil {
		return nil, fmt.Errorf("store '%s' host: %w", id, err)
	}
	client := fetch.NewClient(fetch.Options{Insecure: ep.Insecure, DialTimeout: 3 * time.Second, RequestTimeout: 15 * time.Second})
	return &doctor.ClientStore{Client: client, Host: hostPort, Token: ep.Token}, nil
}
