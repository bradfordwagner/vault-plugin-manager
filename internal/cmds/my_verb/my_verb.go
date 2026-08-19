package my_verb

import (
	"template_cli/internal/args"

	"github.com/bradfordwagner/go-util/log"
)

// Run is the main function for the myVerb command
func Run(a args.MyArgs) (err error) {
	log.Log().With("cmd", "myArgs").With("args", a).Info("hi friends")
	return
}
