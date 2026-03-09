// Package main provides the tfreport CLI tool.
// tfreport reads a Terraform plan JSON file and prints a human-readable
// summary of every resource change, including the changed attributes.
//
// Usage:
//
//	terraform show -json tfplan | tfreport
//	tfreport < plan.json
//	tfreport plan.json
//	tfreport -out summary.txt plan.json
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/laredoute/tfreport/parser"
	"github.com/laredoute/tfreport/reader"
	"github.com/laredoute/tfreport/terraformstate"
	"github.com/laredoute/tfreport/writer"
)

var version = "development"

func main() {
	printVersion := flag.Bool("v", false, "print version")
	outputFileName := flag.String("out", "", "[Optional] write output to file instead of stdout")

	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(),
			"tfreport — human-readable Terraform plan diff\n\n"+
				"Usage: tfreport [flags] [plan.json]\n"+
				"       terraform show -json tfplan | tfreport\n\n"+
				"Flags:\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if *printVersion {
		fmt.Fprintf(os.Stdout, "Version: %s\n", version)
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) > 1 {
		fmt.Fprintf(os.Stderr, "error: only one argument allowed (plan file), got %v\n", args)
		flag.Usage()
		os.Exit(1)
	}

	newReader, err := reader.CreateReader(args)
	logIfErrorAndExit("error creating input reader: %s", err)

	input, err := newReader.Read()
	logIfErrorAndExit("error reading input: %s", err)

	newParser, err := parser.CreateParser(input, newReader.Name())
	logIfErrorAndExit("error creating parser: %s", err)

	terraformState, err := newParser.Parse()
	logIfErrorAndExit("error parsing plan: %s", err)

	terraformstate.FilterNoOpResources(&terraformState)

	// Details mode is the only mode in tfreport.
	w := writer.NewReportWriter(terraformState)

	var out io.Writer = os.Stdout
	if *outputFileName != "" {
		file, err := os.OpenFile(*outputFileName, os.O_TRUNC|os.O_CREATE|os.O_WRONLY, 0600)
		logIfErrorAndExit("error opening output file: %s", err)
		defer file.Close()
		out = file
	}

	err = w.Write(out)
	logIfErrorAndExit("error writing report: %s", err)

	if *outputFileName != "" {
		fmt.Fprintf(os.Stderr, "Written plan report to %s\n", *outputFileName)
	}
}

func logIfErrorAndExit(format string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, format+"\n", err.Error())
		os.Exit(1)
	}
}
