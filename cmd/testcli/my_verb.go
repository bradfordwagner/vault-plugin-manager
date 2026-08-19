package main

import (
	"template_cli/internal/args"
	"template_cli/internal/cmds/my_verb"

	"github.com/bradfordwagner/go-util/flag_helper"
	"github.com/spf13/cobra"
)

func init() {
	fs := myVerb.Flags()
	flag_helper.CreateFlag(fs, &myArgs.HelloWorld, "hello_world", "w", "default_value", "hello world")
}

var myArgs args.MyArgs

var myVerb = &cobra.Command{
	Use:   "myVerb",
	Short: "myVerb does something",
	ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{}, cobra.ShellCompDirectiveDefault
	},
	RunE: func(cmd *cobra.Command, args []string) (err error) {
		flag_helper.Load(&myArgs)
		return my_verb.Run(myArgs)
	},
}
