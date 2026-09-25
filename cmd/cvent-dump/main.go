// Command cvent-dump fetches one Cvent event's full data bundle and writes it
// as a snapshot. It is a thin, dependency-free CLI over the cvent package:
// credentials come from the environment or a .env file (see the package
// docs), and the same dump layout the PWA server's --dump mode produces.
//
// Usage:
//
//	cvent-dump [event-code-or-uuid] [flags]
//
// The event code/uuid may be given as the first positional argument or via
// CVENT_EVENT. If neither is set, the build default is used.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/zhangt58/cvent/cvent"
)

const defaultEventID = "TESTCODE01"

func main() {
	var (
		dir     = flag.String("dir", "data", "output directory for the snapshot")
		envFile = flag.String("env-file", "", "explicit .env file for credentials (overrides the repo-root walk-up; the real environment still wins for any key it sets)")
		apiBase = flag.String("api-base", "", "Cvent API base URL (overrides CVENT_API_BASE)")
	)
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: %s [event-code-or-uuid] [flags]\n\n", os.Args[0])
		fmt.Fprintln(flag.CommandLine.Output(), "Flags:")
		flag.PrintDefaults()
	}
	flag.Parse()

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

	event := ""
	if flag.NArg() >= 1 {
		event = flag.Arg(0)
	}
	if event == "" {
		event = os.Getenv("CVENT_EVENT")
	}
	if event == "" {
		event = defaultEventID
	}

	summary, err := cvent.RunDump(creds, event, *dir)
	if err != nil {
		fail("dump: " + err.Error())
	}
	fmt.Println(summary)
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "cvent-dump: "+msg)
	os.Exit(1)
}
