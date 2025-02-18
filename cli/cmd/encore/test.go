package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"encr.dev/cli/cmd/encore/cmdutil"
	daemonpb "encr.dev/proto/encore/daemon"
)

var testCmd = &cobra.Command{
	Use:   "test [go test flags]",
	Short: "Tests your application",
	Long:  "Takes all the same flags as `go test`.",

	DisableFlagsInUseLine: true,
	Run: func(cmd *cobra.Command, args []string) {
		var (
			traceFile    string
			codegenDebug bool
			prepareOnly  bool
			noColor      bool
		)
		// Support specific args but otherwise let all args be passed on to "go test"
		for i := 0; i < len(args); i++ {
			arg := args[i]
			if arg == "-h" || arg == "--help" {
				_ = cmd.Help()
				return
			} else if arg == "--trace" || strings.HasPrefix(arg, "--trace=") {
				// Drop this argument always.
				args = slices.Delete(args, i, i+1)
				i--

				// We either have '--trace=file' or '--trace file'.
				// Handle both.
				if _, value, ok := strings.Cut(arg, "="); ok {
					traceFile = value
				} else {
					// Make sure there is a next argument.
					if i < len(args) {
						traceFile = args[i]
						args = slices.Delete(args, i, i+1)
						i--
					}
				}
			} else if arg == "--codegen-debug" {
				codegenDebug = true
				args = slices.Delete(args, i, i+1)
				i--
			} else if arg == "--prepare" {
				prepareOnly = true
				args = slices.Delete(args, i, i+1)
				i--
			} else if arg == "--no-color" {
				noColor = true
				args = slices.Delete(args, i, i+1)
				i--
			}
		}

		appRoot, relPath := determineAppRoot()
		runTests(appRoot, relPath, args, traceFile, codegenDebug, prepareOnly, noColor)
	},
}

func runTests(appRoot, testDir string, args []string, traceFile string, codegenDebug, prepareOnly, noColor bool) {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-interrupt
		cancel()
	}()

	converter := cmdutil.ConvertJSONLogs(cmdutil.Colorize(!noColor))
	if slices.Contains(args, "-json") {
		converter = convertTestEventOutputOnly(converter)
	}

	daemon := setupDaemon(ctx)

	// Is this a node package?
	packageJsonPath := filepath.Join(appRoot, "package.json")
	if _, err := os.Stat(packageJsonPath); err == nil || prepareOnly {
		spec, err := daemon.TestSpec(ctx, &daemonpb.TestSpecRequest{
			AppRoot:    appRoot,
			WorkingDir: testDir,
			Args:       args,
			Environ:    os.Environ(),
		})
		if status.Code(err) == codes.NotFound {
			fatal("application does not define any tests.\nNote: Add a 'test' script command to package.json to run tests.")
		} else if err != nil {
			fatal(err)
		}

		if prepareOnly {
			for _, ln := range spec.Environ {
				fmt.Println(ln)
			}
			return
		}

		cmd := exec.Command(spec.Command, spec.Args...)
		cmd.Env = spec.Environ
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Stdin = os.Stdin

		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				os.Exit(exitErr.ExitCode())
			} else {
				fatal(err)
			}
		}
		return
	}

	stream, err := daemon.Test(ctx, &daemonpb.TestRequest{
		AppRoot:      appRoot,
		WorkingDir:   testDir,
		Args:         args,
		Environ:      os.Environ(),
		TraceFile:    nonZeroPtr(traceFile),
		CodegenDebug: codegenDebug,
	})
	if err != nil {
		fatal(err)
	}
	os.Exit(cmdutil.StreamCommandOutput(stream, converter))
}

func init() {
	testCmd.DisableFlagParsing = true
	rootCmd.AddCommand(testCmd)

	// Even though we've disabled flag parsing, we still need to define the flags
	// so that the help text is correct.
	testCmd.Flags().Bool("codegen-debug", false, "Dump generated code (for debugging Encore's code generation)")
	testCmd.Flags().Bool("prepare", false, "Prepare for running tests (without running them)")
	testCmd.Flags().String("trace", "", "Specifies a trace file to write trace information about the parse and compilation process to.")
	testCmd.Flags().Bool("no-color", false, "Disable colorized output")

}

func convertTestEventOutputOnly(converter cmdutil.OutputConverter) cmdutil.OutputConverter {
	return func(line []byte) []byte {
		// If this isn't a JSON log line, just return it as-is
		if len(line) == 0 || line[0] != '{' {
			return line
		}

		testEvent := &testJSONEvent{}
		if err := json.Unmarshal(line, testEvent); err == nil && testEvent.Action == "output" {
			if testEvent.Output != nil && (*(testEvent.Output))[0] == '{' {
				convertedLogs := textBytes(converter(*testEvent.Output))
				testEvent.Output = &convertedLogs

				newLine, err := json.Marshal(testEvent)
				if err == nil {
					return append(newLine, '\n')
				}
			}
		}

		return line
	}
}

// testJSONEvent and textBytes taken from the Go source code
type testJSONEvent struct {
	Time    *time.Time `json:",omitempty"`
	Action  string
	Package string     `json:",omitempty"`
	Test    string     `json:",omitempty"`
	Elapsed *float64   `json:",omitempty"`
	Output  *textBytes `json:",omitempty"`
}

// textBytes is a hack to get JSON to emit a []byte as a string
// without actually copying it to a string.
// It implements encoding.TextMarshaler, which returns its text form as a []byte,
// and then json encodes that text form as a string (which was our goal).
type textBytes []byte

func (b *textBytes) MarshalText() ([]byte, error) { return *b, nil }
func (b *textBytes) UnmarshalText(in []byte) error {
	*b = in
	return nil
}
