// Package ui provides the console prompts used by `init` and the confirmations
// used by destructive commands.
package ui

import (
	"bufio"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Prompter reads answers from a terminal.
type Prompter struct {
	In  *bufio.Reader
	Out io.Writer
}

// New returns a prompter on stdin/stdout.
func New() *Prompter {
	return &Prompter{In: bufio.NewReader(os.Stdin), Out: os.Stdout}
}

func (p *Prompter) printf(format string, a ...any) {
	fmt.Fprintf(p.Out, format, a...)
}

// Section prints a heading, so a long wizard stays navigable.
func (p *Prompter) Section(title string) {
	p.printf("\n\033[1m%s\033[0m\n%s\n", title, strings.Repeat("─", len(title)))
}

// Info prints an explanatory line.
func (p *Prompter) Info(format string, a ...any) {
	p.printf("  "+format+"\n", a...)
}

// Warn prints a line that should not be skimmed past.
func (p *Prompter) Warn(format string, a ...any) {
	p.printf("  \033[33m!\033[0m "+format+"\n", a...)
}

// Ask reads a line, returning def when the answer is empty.
func (p *Prompter) Ask(question, def string) (string, error) {
	if def != "" {
		p.printf("  %s [%s]: ", question, def)
	} else {
		p.printf("  %s: ", question)
	}
	line, err := p.In.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	answer := strings.TrimSpace(line)
	if answer == "" {
		return def, nil
	}
	return answer, nil
}

// AskRequired keeps asking until a non-empty answer arrives.
func (p *Prompter) AskRequired(question string) (string, error) {
	for {
		v, err := p.Ask(question, "")
		if err != nil {
			return "", err
		}
		if v != "" {
			return v, nil
		}
		p.Warn("this one is required")
	}
}

// AskInt reads an integer within [min,max].
func (p *Prompter) AskInt(question string, def, min, max int) (int, error) {
	for {
		v, err := p.Ask(question, strconv.Itoa(def))
		if err != nil {
			return 0, err
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(v))
		if convErr != nil {
			p.Warn("%q is not a number", v)
			continue
		}
		if n < min || n > max {
			p.Warn("must be between %d and %d", min, max)
			continue
		}
		return n, nil
	}
}

// Confirm asks a yes/no question.
func (p *Prompter) Confirm(question string, def bool) (bool, error) {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		v, err := p.Ask(fmt.Sprintf("%s (%s)", question, hint), "")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		p.Warn("please answer y or n")
	}
}

// Choose presents a numbered menu and returns the chosen index.
func (p *Prompter) Choose(question string, options []string, def int) (int, error) {
	p.printf("  %s\n", question)
	for i, o := range options {
		marker := " "
		if i == def {
			marker = "*"
		}
		p.printf("   %s %d) %s\n", marker, i+1, o)
	}
	for {
		v, err := p.Ask("choice", strconv.Itoa(def+1))
		if err != nil {
			return 0, err
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(v))
		if convErr != nil || n < 1 || n > len(options) {
			p.Warn("pick a number between 1 and %d", len(options))
			continue
		}
		return n - 1, nil
	}
}

// AskSecret reads a value without echoing it where the terminal allows that.
func (p *Prompter) AskSecret(question string) (string, error) {
	restore, quiet := disableEcho()
	if !quiet {
		p.printf("  (this terminal will show what you type)\n")
	}
	p.printf("  %s: ", question)
	line, err := p.In.ReadString('\n')
	restore()
	p.printf("\n")
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// disableEcho turns off terminal echo when it can, returning a restore func and
// whether echo was actually suppressed.
func disableEcho() (restore func(), quiet bool) {
	if runtime.GOOS == "windows" {
		return func() {}, false
	}
	if err := sttyRun("-echo"); err != nil {
		return func() {}, false
	}
	return func() { _ = sttyRun("echo") }, true
}

func sttyRun(arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

// GeneratePassword returns a high-entropy, URL-safe secret.
// 32 bytes is well past what any brute force reaches; the practical risk to a
// backup password is losing it, not guessing it.
func GeneratePassword(nBytes int) (string, error) {
	if nBytes <= 0 {
		nBytes = 32
	}
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
