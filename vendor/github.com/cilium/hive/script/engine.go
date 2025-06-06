// Copyright 2022 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package script implements a small, customizable, platform-agnostic scripting
// language.
//
// Scripts are run by an [Engine] configured with a set of available commands
// and conditions that guard those commands. Each script has an associated
// working directory and environment, along with a buffer containing the stdout
// and stderr output of a prior command, tracked in a [State] that commands can
// inspect and modify.
//
// The default commands configured by [NewEngine] resemble a simplified Unix
// shell.
//
// # Script Language
//
// Each line of a script is parsed into a sequence of space-separated command
// words, with environment variable expansion within each word and # marking an
// end-of-line comment. Additional variables named ':' and '/' are expanded
// within script arguments (expanding to the value of os.PathListSeparator and
// os.PathSeparator respectively) but are not inherited in subprocess
// environments.
//
// Adding single quotes around text keeps spaces in that text from being treated
// as word separators and also disables environment variable expansion.
// Inside a single-quoted block of text, a repeated single quote indicates
// a literal single quote, as in:
//
//	'Don''t communicate by sharing memory.'
//
// A line beginning with # is a comment and conventionally explains what is
// being done or tested at the start of a new section of the script.
//
// Commands are executed one at a time, and errors are checked for each command;
// if any command fails unexpectedly, no subsequent commands in the script are
// executed. The command prefix ! indicates that the command on the rest of the
// line (typically go or a matching predicate) must fail instead of succeeding.
// The command prefix ? indicates that the command may or may not succeed, but
// the script should continue regardless.
//
// The command prefix [cond] indicates that the command on the rest of the line
// should only run when the condition is satisfied.
//
// A condition can be negated: [!root] means to run the rest of the line only if
// the user is not root. Multiple conditions may be given for a single command,
// for example, '[linux] [amd64] skip'. The command will run if all conditions
// are satisfied.
//
// Package script is particularly good for writing tests.
// Ironically, it has no tests.
package script

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

// An Engine stores the configuration for executing a set of scripts.
//
// The same Engine may execute multiple scripts concurrently.
type Engine struct {
	Cmds  map[string]Cmd
	Conds map[string]Cond

	// If Quiet is true, Execute deletes log prints from the previous
	// section when starting a new section.
	Quiet bool

	// RetryInterval is the minimal interval for retrying commands marked with '*'.
	// If zero, then the default retry interval is used.
	RetryInterval time.Duration

	// MaxRetryInterval is the maximum time to wait before retrying.
	MaxRetryInterval time.Duration
}

// NewEngine returns an Engine configured with a basic set of commands and conditions.
func NewEngine() *Engine {
	return &Engine{
		Cmds:             DefaultCmds(),
		Conds:            DefaultConds(),
		RetryInterval:    defaultRetryInterval,
		MaxRetryInterval: defaultMaxRetryInterval,
	}
}

const (
	defaultRetryInterval    = 100 * time.Millisecond
	defaultMaxRetryInterval = 500 * time.Millisecond
)

// A Cmd is a command that is available to a script.
type Cmd interface {
	// Run begins running the command.
	//
	// If the command produces output or can be run in the background, run returns
	// a WaitFunc that will be called to obtain the result of the command and
	// update the engine's stdout and stderr buffers.
	//
	// Run itself and the returned WaitFunc may inspect and/or modify the State,
	// but the State's methods must not be called concurrently after Run has
	// returned.
	//
	// Run may retain and access the args slice until the WaitFunc has returned.
	Run(s *State, args ...string) (WaitFunc, error)

	// Usage returns the usage for the command, which the caller must not modify.
	Usage() *CmdUsage
}

// A WaitFunc is a function called to retrieve the results of a Cmd.
type WaitFunc func(*State) (stdout, stderr string, err error)

// A CmdUsage describes the usage of a Cmd, independent of its name
// (which can change based on its registration).
type CmdUsage struct {
	Summary string // in the style of the Name section of a Unix 'man' page, omitting the name

	// synopsis of arguments, e.g. "files...". If [Flags] is provided then these will be prepended
	// to [Args] in usage output.
	Args string

	// flags for the command, optional. If provided, then args passed to the command will not include any
	// of the flag arguments. The [plafg.FlagSet] is available to the command via [State.Flags].
	Flags func(fs *pflag.FlagSet)

	Detail []string // zero or more sentences in the style of the Description section of a Unix 'man' page

	// If Async is true, the Cmd is meaningful to run in the background, and its
	// Run method must return either a non-nil WaitFunc or a non-nil error.
	Async bool

	// RegexpArgs reports which arguments, if any, should be treated as regular
	// expressions. It takes as input the raw, unexpanded arguments and returns
	// the list of argument indices that will be interpreted as regular
	// expressions.
	//
	// If RegexpArgs is nil, all arguments are assumed not to be regular
	// expressions.
	RegexpArgs func(rawArgs ...string) []int
}

// A Cond is a condition deciding whether a command should be run.
type Cond interface {
	// Eval reports whether the condition applies to the given State.
	//
	// If the condition's usage reports that it is a prefix,
	// the condition must be used with a suffix.
	// Otherwise, the passed-in suffix argument is always the empty string.
	Eval(s *State, suffix string) (bool, error)

	// Usage returns the usage for the condition, which the caller must not modify.
	Usage() *CondUsage
}

// A CondUsage describes the usage of a Cond, independent of its name
// (which can change based on its registration).
type CondUsage struct {
	Summary string // a single-line summary of when the condition is true

	// If Prefix is true, the condition is a prefix and requires a
	// colon-separated suffix (like "[GOOS:linux]" for the "GOOS" condition).
	// The suffix may be the empty string (like "[prefix:]").
	Prefix bool
}

var ParseError = errors.New("parse error")

// Execute reads and executes script, writing the output to log.
//
// Execute stops and returns an error at the first command that does not succeed.
// The returned error's text begins with "file:line: ".
//
// If the script runs to completion or ends by a 'stop' command,
// Execute returns nil.
//
// Execute does not stop background commands started by the script
// before returning. To stop those, use [State.CloseAndWait] or the
// [Wait] command.
func (e *Engine) Execute(s *State, file string, script *bufio.Reader, log io.Writer) (err error) {
	defer func(prev *Engine) { s.engine = prev }(s.engine)
	s.engine = e
	defer func(prev io.Writer) { s.logOut = prev }(s.logOut)
	s.logOut = log

	retryInterval := e.RetryInterval
	if retryInterval == 0 {
		retryInterval = defaultRetryInterval
	}
	maxRetryInterval := e.MaxRetryInterval
	if maxRetryInterval == 0 {
		maxRetryInterval = defaultMaxRetryInterval
	}

	var (
		sectionStart time.Time
		sectionCmds  []*command
	)
	// endSection flushes the logs for the current section from s.log to log.
	// ok indicates whether all commands in the section succeeded.
	endSection := func(ok bool) error {
		var err error
		if sectionStart.IsZero() {
			// We didn't write a section header or record a timestamp, so just dump the
			// whole log without those.
			if s.log.Len() > 0 {
				err = s.FlushLog()
			}
		} else if s.log.Len() == 0 {
			// Adding elapsed time for doing nothing is meaningless, so don't.
			_, err = io.WriteString(log, "\n")
		} else {
			// Insert elapsed time for section at the end of the section's comment.
			_, err = fmt.Fprintf(log, " (%.3fs)\n", time.Since(sectionStart).Seconds())

			if err == nil && (!ok || !e.Quiet) {
				err = s.FlushLog()
			} else {
				s.log.Reset()
			}
		}

		sectionStart = time.Time{}
		sectionCmds = nil
		return err
	}

	var lineno int
	lineErr := func(err error) error {
		if errors.As(err, new(*CommandError)) {
			return err
		}
		return fmt.Errorf("%s:%d: %w", file, lineno, err)
	}

	// In case of failure or panic, flush any pending logs for the section.
	defer func() {
		if sErr := endSection(false); sErr != nil && err == nil {
			err = lineErr(sErr)
		}
	}()

	for {
		if err := s.ctx.Err(); err != nil {
			// This error wasn't produced by any particular command,
			// so don't wrap it in a CommandError.
			return lineErr(err)
		}

		line, err := script.ReadString('\n')
		if err == io.EOF {
			if line == "" {
				break // Reached the end of the script.
			}
			// If the script doesn't end in a newline, interpret the final line.
		} else if err != nil {
			return lineErr(err)
		}
		line = strings.TrimSuffix(line, "\n")
		lineno++

		// The comment character "#" at the start of the line delimits a section of
		// the script.
		if strings.HasPrefix(line, "#") {
			// If there was a previous section, the fact that we are starting a new
			// one implies the success of the previous one.
			//
			// At the start of the script, the state may also contain accumulated logs
			// from commands executed on the State outside of the engine in order to
			// set it up; flush those logs too.
			if err := endSection(true); err != nil {
				return lineErr(err)
			}

			// Log the section start without a newline so that we can add
			// a timestamp for the section when it ends.
			_, err = fmt.Fprintf(log, "%s", line)
			sectionStart = time.Now()
			if err != nil {
				return lineErr(err)
			}
			continue
		}

		cmd, err := parse(file, lineno, line)
		if cmd == nil && err == nil {
			continue // Ignore blank lines.
		}
		sectionCmds = append(sectionCmds, cmd)

		s.Logf("> %s\n", line)
		if err != nil {
			return lineErr(fmt.Errorf("%w: %w", ParseError, err))
		}

		// Evaluate condition guards.
		ok, err := e.conditionsActive(s, cmd.conds)
		if err != nil {
			return lineErr(err)
		}
		if !ok {
			s.Logf("[condition not met]\n")
			continue
		}

		impl := e.Cmds[cmd.name]

		// Expand variables in arguments.
		var regexpArgs []int
		if impl != nil {
			usage := impl.Usage()
			if usage.RegexpArgs != nil {
				// First join rawArgs without expansion to pass to RegexpArgs.
				rawArgs := make([]string, 0, len(cmd.rawArgs))
				for _, frags := range cmd.rawArgs {
					var b strings.Builder
					for _, frag := range frags {
						b.WriteString(frag.s)
					}
					rawArgs = append(rawArgs, b.String())
				}
				regexpArgs = usage.RegexpArgs(rawArgs...)
			}
		}
		cmd.origArgs = expandArgs(s, cmd.rawArgs, regexpArgs)
		cmd.args = cmd.origArgs
		s.RetryCount = 0
		s.retriesRequested = cmd.want == successRetryOnFailure || cmd.want == failureRetryOnSuccess

		// Run the command.
		err = e.runCommand(s, cmd, impl)
		if err != nil {
			if s.retriesRequested {
				retryStart := sectionStart

				// Clear the section start to avoid the deferred endSection() printing a timestamp.
				sectionStart = time.Time{}

				// Append the new line that section start omitted and flush the log.
				io.WriteString(log, "\n")
				s.FlushLog()

				// Command wants retries. Retry the whole section
				backoff := exponentialBackoff{max: maxRetryInterval, interval: retryInterval}
				for err != nil {
					retryDuration := backoff.get()
					fmt.Fprintf(log, "(command %q failed, retrying in %s...)\n", line, retryDuration)
					select {
					case <-s.Context().Done():
						s.RetryCount = 0
						return lineErr(s.Context().Err())
					case <-time.After(retryDuration):
					}
					s.RetryCount++
					for _, cmd := range sectionCmds {
						impl := e.Cmds[cmd.name]
						if err = e.runCommand(s, cmd, impl); err != nil {
							break
						}
					}
					s.FlushLog()
				}
				fmt.Fprintf(log, "(command %q succeeded after %d retries in %.3fs)\n", line, s.RetryCount, time.Since(retryStart).Seconds())
				s.RetryCount = 0
			} else {
				if stop := (stopError{}); errors.As(err, &stop) {
					// Since the 'stop' command halts execution of the entire script,
					// log its message separately from the section in which it appears.
					err = endSection(true)
					s.Logf("%v\n", stop)
					if err == nil {
						return nil
					}
				}
				return lineErr(err)
			}
		}
	}

	if err := endSection(true); err != nil {
		return lineErr(err)
	}
	return nil
}

func (e *Engine) ExecuteLine(s *State, line string, log io.Writer) (err error) {
	defer func(prev *Engine) { s.engine = prev }(s.engine)
	s.engine = e
	defer func(prev io.Writer) { s.logOut = prev }(s.logOut)
	s.logOut = log
	defer s.FlushLog()

	cmd, err := parse("<stdin>", 0, line)
	if cmd == nil && err == nil {
		return nil
	}
	if err != nil {
		return err
	}

	// Evaluate condition guards.
	ok, err := e.conditionsActive(s, cmd.conds)
	if err != nil {
		return err
	}
	if !ok {
		s.Logf("[condition not met]\n")
		return
	}

	impl := e.Cmds[cmd.name]

	// Expand variables in arguments.
	var regexpArgs []int
	if impl != nil {
		usage := impl.Usage()
		if usage.RegexpArgs != nil {
			// First join rawArgs without expansion to pass to RegexpArgs.
			rawArgs := make([]string, 0, len(cmd.rawArgs))
			for _, frags := range cmd.rawArgs {
				var b strings.Builder
				for _, frag := range frags {
					b.WriteString(frag.s)
				}
				rawArgs = append(rawArgs, b.String())
			}
			regexpArgs = usage.RegexpArgs(rawArgs...)
		}
	}
	cmd.args = expandArgs(s, cmd.rawArgs, regexpArgs)
	cmd.origArgs = cmd.args

	// Run the command.
	err = e.runCommand(s, cmd, impl)
	if err != nil {
		if stop := (stopError{}); errors.As(err, &stop) {
			// Since the 'stop' command halts execution of the entire script,
			// log its message separately from the section in which it appears.
			s.Logf("%v\n", stop)
			if err == nil {
				return nil
			}
		}
		return err
	}
	return nil
}

// A command is a complete command parsed from a script.
type command struct {
	file       string
	line       int
	want       expectedStatus
	conds      []condition // all must be satisfied
	name       string      // the name of the command; must be non-empty
	rawArgs    [][]argFragment
	origArgs   []string // original arguments before pflag parsing
	args       []string // shell-expanded arguments following name
	background bool     // command should run in background (ends with a trailing &)
}

// An expectedStatus describes the expected outcome of a command.
// Script execution halts when a command does not match its expected status.
type expectedStatus string

const (
	success               expectedStatus = ""
	failure               expectedStatus = "!"
	successOrFailure      expectedStatus = "?"
	successRetryOnFailure expectedStatus = "*"
	failureRetryOnSuccess expectedStatus = "!*"
)

type argFragment struct {
	s      string
	quoted bool // if true, disable variable expansion for this fragment
}

type condition struct {
	want bool
	tag  string
}

const argSepChars = " \t\r\n#"

// parse parses a single line as a list of space-separated arguments.
// subject to environment variable expansion (but not resplitting).
// Single quotes around text disable splitting and expansion.
// To embed a single quote, double it:
//
//	'Don''t communicate by sharing memory.'
func parse(filename string, lineno int, line string) (cmd *command, err error) {
	cmd = &command{file: filename, line: lineno}
	var (
		rawArg []argFragment // text fragments of current arg so far (need to add line[start:i])
		start  = -1          // if >= 0, position where current arg text chunk starts
		quoted = false       // currently processing quoted text
	)

	flushArg := func() error {
		if len(rawArg) == 0 {
			return nil // Nothing to flush.
		}
		defer func() { rawArg = nil }()

		if cmd.name == "" && len(rawArg) == 1 && !rawArg[0].quoted {
			arg := rawArg[0].s

			// Command prefix ! means negate the expectations about this command:
			// go command should fail, match should not be found, etc.
			// Prefix ? means allow either success or failure.
			// Prefix * means to retry the command until success (or context cancelled)
			// Prefix !* means to retry the command until failure (or context cancelled)
			switch want := expectedStatus(arg); want {
			case failure, successOrFailure, successRetryOnFailure, failureRetryOnSuccess:
				if cmd.want != "" {
					return errors.New("duplicated '!', '?', '*' or '!*' token")
				}
				cmd.want = want
				return nil
			}

			// Command prefix [cond] means only run this command if cond is satisfied.
			if strings.HasPrefix(arg, "[") && strings.HasSuffix(arg, "]") {
				want := true
				arg = strings.TrimSpace(arg[1 : len(arg)-1])
				if strings.HasPrefix(arg, "!") {
					want = false
					arg = strings.TrimSpace(arg[1:])
				}
				if arg == "" {
					return errors.New("empty condition")
				}
				cmd.conds = append(cmd.conds, condition{want: want, tag: arg})
				return nil
			}

			if arg == "" {
				return errors.New("empty command")
			}
			cmd.name = arg
			return nil
		}

		cmd.rawArgs = append(cmd.rawArgs, rawArg)
		return nil
	}

	for i := 0; ; i++ {
		if !quoted && (i >= len(line) || strings.ContainsRune(argSepChars, rune(line[i]))) {
			// Found arg-separating space.
			if start >= 0 {
				rawArg = append(rawArg, argFragment{s: line[start:i], quoted: false})
				start = -1
			}
			if err := flushArg(); err != nil {
				return nil, err
			}
			if i >= len(line) || line[i] == '#' {
				break
			}
			continue
		}
		if i >= len(line) {
			return nil, errors.New("unterminated quoted argument")
		}
		if line[i] == '\'' {
			if !quoted {
				// starting a quoted chunk
				if start >= 0 {
					rawArg = append(rawArg, argFragment{s: line[start:i], quoted: false})
				}
				start = i + 1
				quoted = true
				continue
			}
			// 'foo''bar' means foo'bar, like in rc shell and Pascal.
			if i+1 < len(line) && line[i+1] == '\'' {
				rawArg = append(rawArg, argFragment{s: line[start:i], quoted: true})
				start = i + 1
				i++ // skip over second ' before next iteration
				continue
			}
			// ending a quoted chunk
			rawArg = append(rawArg, argFragment{s: line[start:i], quoted: true})
			start = i + 1
			quoted = false
			continue
		}
		// found character worth saving; make sure we're saving
		if start < 0 {
			start = i
		}
	}

	if cmd.name == "" {
		if cmd.want != "" || len(cmd.conds) > 0 || len(cmd.rawArgs) > 0 || cmd.background {
			// The line contains a command prefix or suffix, but no actual command.
			return nil, errors.New("missing command")
		}

		// The line is blank, or contains only a comment.
		return nil, nil
	}

	if n := len(cmd.rawArgs); n > 0 {
		last := cmd.rawArgs[n-1]
		if len(last) == 1 && !last[0].quoted && last[0].s == "&" {
			cmd.background = true
			cmd.rawArgs = cmd.rawArgs[:n-1]
		}
	}
	return cmd, nil
}

// expandArgs expands the shell variables in rawArgs and joins them to form the
// final arguments to pass to a command.
func expandArgs(s *State, rawArgs [][]argFragment, regexpArgs []int) []string {
	args := make([]string, 0, len(rawArgs))
	for i, frags := range rawArgs {
		isRegexp := false
		for _, j := range regexpArgs {
			if i == j {
				isRegexp = true
				break
			}
		}

		var b strings.Builder
		for _, frag := range frags {
			if frag.quoted {
				b.WriteString(frag.s)
			} else {
				b.WriteString(s.ExpandEnv(frag.s, isRegexp))
			}
		}
		args = append(args, b.String())
	}
	return args
}

// quoteArgs returns a string that parse would parse as args when passed to a command.
//
// TODO(bcmills): This function should have a fuzz test.
func quoteArgs(args []string) string {
	var b strings.Builder
	for i, arg := range args {
		if i > 0 {
			b.WriteString(" ")
		}
		if strings.ContainsAny(arg, "'"+argSepChars) {
			// Quote the argument to a form that would be parsed as a single argument.
			b.WriteString("'")
			b.WriteString(strings.ReplaceAll(arg, "'", "''"))
			b.WriteString("'")
		} else {
			b.WriteString(arg)
		}
	}
	return b.String()
}

func (e *Engine) conditionsActive(s *State, conds []condition) (bool, error) {
	for _, cond := range conds {
		var impl Cond
		prefix, suffix, ok := strings.Cut(cond.tag, ":")
		if ok {
			impl = e.Conds[prefix]
			if impl == nil {
				return false, fmt.Errorf("unknown condition prefix %q", prefix)
			}
			if !impl.Usage().Prefix {
				return false, fmt.Errorf("condition %q cannot be used with a suffix", prefix)
			}
		} else {
			impl = e.Conds[cond.tag]
			if impl == nil {
				return false, fmt.Errorf("unknown condition %q", cond.tag)
			}
			if impl.Usage().Prefix {
				return false, fmt.Errorf("condition %q requires a suffix", cond.tag)
			}
		}
		active, err := impl.Eval(s, suffix)

		if err != nil {
			return false, fmt.Errorf("evaluating condition %q: %w", cond.tag, err)
		}
		if active != cond.want {
			return false, nil
		}
	}

	return true, nil
}

func (e *Engine) runCommand(s *State, cmd *command, impl Cmd) error {
	if impl == nil {
		return cmdError(cmd, errors.New("unknown command"))
	}

	usage := impl.Usage()

	async := usage.Async
	if cmd.background && !async {
		return cmdError(cmd, errors.New("command cannot be run in background"))
	}

	// Register and parse the flags. We do this regardless of whether [usage.Flags]
	// is set in order to handle -h/--help.
	fs := pflag.NewFlagSet(cmd.name, pflag.ContinueOnError)
	fs.Usage = func() {} // Don't automatically handle error
	fs.SetOutput(s.logOut)
	if usage.Flags != nil {
		usage.Flags(fs)
	}
	if err := fs.Parse(cmd.origArgs); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			out := new(strings.Builder)
			err = e.ListCmds(out, true, "^"+cmd.name+"$")
			s.stdout = out.String()
			if s.stdout != "" {
				s.Logf("[stdout]\n%s", s.stdout)
			}
			return err
		}
		if usage.Flags != nil {
			return cmdError(cmd, err)
		}

		// [usage.Flags] wasn't given, so ignore the parsing errors as the
		// command might do its own argument parsing.
	} else {
		cmd.args = fs.Args()
		s.Flags = fs
	}

	wait, runErr := impl.Run(s, cmd.args...)
	if wait == nil {
		if async && runErr == nil {
			return cmdError(cmd, errors.New("internal error: async command returned a nil WaitFunc"))
		}
		return checkStatus(cmd, runErr)
	}
	if runErr != nil {
		return cmdError(cmd, errors.New("internal error: command returned both an error and a WaitFunc"))
	}

	if cmd.background {
		s.background = append(s.background, backgroundCmd{
			command: cmd,
			wait:    wait,
		})
		// Clear stdout and stderr, since they no longer correspond to the last
		// command executed.
		s.stdout = ""
		s.stderr = ""
		return nil
	}

	stdout, stderr, waitErr := wait(s)
	s.stdout = stdout
	s.stderr = stderr
	if stdout != "" {
		s.Logf("[stdout]\n%s", stdout)
	}
	if stderr != "" {
		s.Logf("[stderr]\n%s", stderr)
	}
	if cmdErr := checkStatus(cmd, waitErr); cmdErr != nil {
		return cmdErr
	}
	if waitErr != nil {
		// waitErr was expected (by cmd.want), so log it instead of returning it.
		s.Logf("[%v]\n", waitErr)
	}
	return nil
}

func checkStatus(cmd *command, err error) error {
	if err == nil {
		if cmd.want == failure || cmd.want == failureRetryOnSuccess {
			return cmdError(cmd, ErrUnexpectedSuccess)
		}
		return nil
	}

	if s := (stopError{}); errors.As(err, &s) {
		// This error originated in the Stop command.
		// Propagate it as-is.
		return cmdError(cmd, err)
	}

	if w := (waitError{}); errors.As(err, &w) {
		// This error was surfaced from a background process by a call to Wait.
		// Add a call frame for Wait itself, but ignore its "want" field.
		// (Wait itself cannot fail to wait on commands or else it would leak
		// processes and/or goroutines — so a negative assertion for it would be at
		// best ambiguous.)
		return cmdError(cmd, err)
	}

	if cmd.want == success || cmd.want == successRetryOnFailure {
		return cmdError(cmd, err)
	}

	if (cmd.want == failure || cmd.want == failureRetryOnSuccess) && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) {
		// The command was terminated because the script is no longer interested in
		// its output, so we don't know what it would have done had it run to
		// completion — for all we know, it could have exited without error if it
		// ran just a smidge faster.
		return cmdError(cmd, err)
	}

	return nil
}

// ListCmds prints to w a list of the named commands,
// annotating each with its arguments and a short usage summary.
// If verbose is true, ListCmds prints full details for each command.
func (e *Engine) ListCmds(w io.Writer, verbose bool, regexMatch string) error {
	var re *regexp.Regexp
	if regexMatch != "" {
		var err error
		re, err = regexp.Compile(regexMatch)
		if err != nil {
			return err
		}
	}
	names := make([]string, 0, len(e.Cmds))
	for name := range e.Cmds {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if re != nil && !re.MatchString(name) {
			continue
		}
		cmd, ok := e.Cmds[name]
		if !ok {
			_, err := fmt.Fprintf(w, "command %q is not registered\n", name)
			if err != nil {
				return err
			}
			return nil
		}

		usage := cmd.Usage()

		suffix := ""
		if usage.Async {
			suffix = " [&]"
		}

		flagArgs := ""
		flagUsages := ""
		if usage.Flags != nil {
			fs := pflag.NewFlagSet(name, pflag.ContinueOnError)
			fs.SetOutput(w)
			usage.Flags(fs)
			flagUsages = fs.FlagUsages()
			shortArgs := []string{}
			longArgs := []string{}
			fs.VisitAll(func(flag *pflag.Flag) {
				if flag.Shorthand != "" {
					shortArgs = append(shortArgs, flag.Shorthand)
				}
				switch flag.Value.Type() {
				case "bool":
					longArgs = append(longArgs, fmt.Sprintf("[--%s]", flag.Name))
				default:
					longArgs = append(longArgs, fmt.Sprintf("[--%s=%s]", flag.Name, flag.Value.Type()))
				}
			})
			if len(shortArgs) > 0 {
				flagArgs = fmt.Sprintf("[-%s] ", strings.Join(shortArgs, ""))
			}
			flagArgs += strings.Join(longArgs, " ") + " "
		}

		_, err := fmt.Fprintf(w, "%s %s%s\n\t%s\n", name, flagArgs+usage.Args, suffix, usage.Summary)
		if err != nil {
			return err
		}

		if verbose {
			if len(usage.Detail) > 0 {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}

				for _, line := range usage.Detail {
					if err := wrapLine(w, line, 60, "\t"); err != nil {
						return err
					}
				}
			}
			if flagUsages != "" {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
				lines := strings.Split(flagUsages, "\n")
				if len(lines) > 0 {
					if _, err := fmt.Fprintf(w, "\tFlags:\n"); err != nil {
						return err
					}
					for _, line := range lines {
						if _, err := fmt.Fprintf(w, "\t%s\n", line); err != nil {
							return err
						}
					}
				}
			}
		}
	}

	return nil
}

func wrapLine(w io.Writer, line string, cols int, indent string) error {
	line = strings.TrimLeft(line, " ")
	for len(line) > cols {
		bestSpace := -1
		for i, r := range line {
			if r == ' ' {
				if i <= cols || bestSpace < 0 {
					bestSpace = i
				}
				if i > cols {
					break
				}
			}
		}
		if bestSpace < 0 {
			break
		}

		if _, err := fmt.Fprintf(w, "%s%s\n", indent, line[:bestSpace]); err != nil {
			return err
		}
		line = line[bestSpace+1:]
	}

	_, err := fmt.Fprintf(w, "%s%s\n", indent, line)
	return err
}

// ListConds prints to w a list of conditions, one per line,
// annotating each with a description and whether the condition
// is true in the state s (if s is non-nil).
//
// Each of the tag arguments should be a condition string of
// the form "name" or "name:suffix". If no tags are passed as
// arguments, ListConds lists all conditions registered in
// the engine e.
func (e *Engine) ListConds(w io.Writer, s *State, tags ...string) error {
	if tags == nil {
		tags = make([]string, 0, len(e.Conds))
		for name := range e.Conds {
			tags = append(tags, name)
		}
		sort.Strings(tags)
	}

	for _, tag := range tags {
		if prefix, suffix, ok := strings.Cut(tag, ":"); ok {
			cond := e.Conds[prefix]
			if cond == nil {
				return fmt.Errorf("unknown condition prefix %q", prefix)
			}
			usage := cond.Usage()
			if !usage.Prefix {
				return fmt.Errorf("condition %q cannot be used with a suffix", prefix)
			}

			activeStr := ""
			if s != nil {
				if active, _ := cond.Eval(s, suffix); active {
					activeStr = " (active)"
				}
			}
			_, err := fmt.Fprintf(w, "[%s]%s\n\t%s\n", tag, activeStr, usage.Summary)
			if err != nil {
				return err
			}
			continue
		}

		cond := e.Conds[tag]
		if cond == nil {
			return fmt.Errorf("unknown condition %q", tag)
		}
		var err error
		usage := cond.Usage()
		if usage.Prefix {
			_, err = fmt.Fprintf(w, "[%s:*]\n\t%s\n", tag, usage.Summary)
		} else {
			activeStr := ""
			if s != nil {
				if ok, _ := cond.Eval(s, ""); ok {
					activeStr = " (active)"
				}
			}
			_, err = fmt.Fprintf(w, "[%s]%s\n\t%s\n", tag, activeStr, usage.Summary)
		}
		if err != nil {
			return err
		}
	}

	return nil
}

type exponentialBackoff struct {
	max      time.Duration
	interval time.Duration
}

func (eb *exponentialBackoff) get() time.Duration {
	d := eb.interval
	eb.interval = min(eb.interval*2, eb.max)
	return d
}
