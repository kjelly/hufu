package main

import (
	"context"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kjelly/hufu/internal/team"
)

func run(ctx context.Context, args []string, input io.Reader, output, diagnostic io.Writer) error {
	flags := flag.NewFlagSet("diagnostic-fixture", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	eval := flags.Bool("evaluate", false, "emit a paired synthetic evaluation report")
	traces := flags.Bool("traces", false, "emit the 100,000 paired corpus rows as JSONL (requires --evaluate)")
	shadowWorkspace := flags.String("shadow-workspace", "", "persist offline fixture shadow in a dedicated workspace")
	fixtureName := flags.String("fixture", "", "embedded shadow fixture: pass-pass, pass-fail, fail-pass or fail-fail")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *traces && !*eval {
		return fmt.Errorf("unexpected arguments or --traces without --evaluate")
	}
	if *shadowWorkspace != "" && *eval || *fixtureName != "" && *shadowWorkspace == "" {
		return fmt.Errorf("shadow options require --shadow-workspace and cannot be combined with --evaluate")
	}
	var result any
	if *shadowWorkspace != "" {
		name := *fixtureName
		if name == "" {
			name = "pass-pass"
		}
		var err error
		result, err = runFixtureShadow(ctx, *shadowWorkspace, name)
		if err != nil {
			return err
		}
	} else if *eval {
		var rows io.Writer
		if *traces {
			rows = output
		}
		report, err := evaluate(ctx, rows)
		if err != nil {
			return err
		}
		if *traces {
			return nil
		}
		result = report
	} else {
		data, err := io.ReadAll(io.LimitReader(input, (32<<10)+1))
		if err != nil || len(data) > 32<<10 {
			return fmt.Errorf("action input unavailable or oversized")
		}
		var action team.Action
		if err := json.Unmarshal(data, &action, json.RejectUnknownMembers(true)); err != nil {
			return fmt.Errorf("invalid action envelope")
		}
		p, _, _, err := loadOfflineInputs()
		if err != nil {
			return err
		}
		result, err = p.Execute(ctx, action)
		if err != nil {
			return err
		}
	}
	data, err := json.Marshal(result, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = output.Write(append(data, '\n'))
	return err
}

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
