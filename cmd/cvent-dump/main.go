// Command cvent-dump fetches Cvent events' full data bundles and writes them
// as snapshots. It is a thin, dependency-free CLI over the cvent package:
// credentials come from the environment or a .env file (see the package
// docs), and the same dump layout the PWA server's --dump mode produces.
//
// Usage:
//
//	cvent-dump [flags] [event-code-or-uuid ...]
//
// Event codes/uuids may be given as positional arguments; otherwise every
// CVENT_CODE_<n> from the environment / .env is dumped.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/IndicoDataFusion/cvent-data/cvent"
)

func main() {
	var (
		dir     = flag.String("dir", "data", "output directory for the snapshot")
		envFile = flag.String("env-file", "", "explicit .env file for credentials (overrides the repo-root walk-up; the real environment still wins for any key it sets)")
		apiBase = flag.String("api-base", "", "Cvent API base URL (overrides CVENT_API_BASE)")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [flags] [event-code-or-uuid ...]\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(), "Flags:")
		flag.PrintDefaults()
	}
	// Codes and flags may interleave (`cvent-dump CODE --dir data`): the flag
	// package stops at the first positional, so resume parsing after each one.
	var events []string
	for args := os.Args[1:]; ; {
		if err := flag.CommandLine.Parse(args); err != nil {
			os.Exit(2)
		}
		if flag.NArg() == 0 {
			break
		}
		events = append(events, flag.Arg(0))
		args = flag.Args()[1:]
	}

	// Credential source, in precedence order:
	//  1. the real environment (CVENT_CLIENT_ID / CVENT_CLIENT_SECRET /
	//     CVENT_API_BASE) — always wins;
	//  2. an explicit --env-file, if given (its CVENT_* values are used only
	//     for keys the environment does not already set);
	//  3. the repo-root .env, walked up from the working directory (the
	//     package default).
	//
	// Secret values never appear in any error or log line — the package only
	// ever names the variable.
	if *envFile != "" {
		b, err := os.ReadFile(*envFile)
		if err != nil {
			fail(fmt.Sprintf("reading --env-file %s: %v", *envFile, err))
		}
		for k, v := range cvent.ParseDotEnv(b) {
			if strings.HasPrefix(k, "CVENT_") && os.Getenv(k) == "" {
				os.Setenv(k, v)
			}
		}
	}

	creds, err := cvent.FromEnvironment()
	if err != nil {
		fail(err.Error())
	}
	if *apiBase != "" {
		creds.BaseURL = *apiBase
	}

	if len(events) == 0 {
		events = cvent.EventCodes()
	}
	if len(events) == 0 {
		fail("no events: pass event codes/uuids or set CVENT_CODE_1 (…) in .env")
	}

	for _, event := range events {
		summary, err := cvent.RunDump(creds, event, *dir)
		if err != nil {
			fail("dump " + event + ": " + err.Error())
		}
		fmt.Println(summary)
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "cvent-dump: "+msg)
	os.Exit(1)
}
