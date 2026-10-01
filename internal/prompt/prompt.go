// Package prompt asks the developer questions on a terminal: free text with
// a default, yes/no, and a choice from a list, each re-asked on invalid
// input. It reads lines from an io.Reader so tests can script the answers.
package prompt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Prompter asks questions read line by line from R and written to W.
type Prompter struct {
	r *bufio.Reader
	w io.Writer
}

// New builds a Prompter reading from r and writing to w. r is wrapped in a
// bufio.Reader, so it should not be read from elsewhere afterwards.
func New(r io.Reader, w io.Writer) *Prompter {
	return &Prompter{r: bufio.NewReader(r), w: w}
}

// errNoAnswer is returned when the input runs out. A terminal never does, so
// in practice it means a test's scripted input ran out.
var errNoAnswer = errors.New("prompt: no more input")

// readLine reads one line, trimmed, or errNoAnswer if none is left.
func (p *Prompter) readLine() (string, error) {
	line, err := p.r.ReadString('\n')
	if err != nil {
		if !errors.Is(err, io.EOF) {
			return "", err
		}
		if line == "" {
			return "", errNoAnswer
		}
	}
	return strings.TrimSpace(line), nil
}

// Text asks question, showing def in brackets when it is not empty. A blank
// answer takes def. When validate is not nil, it runs on the resulting
// answer; a non-nil error is printed and the question asked again.
func (p *Prompter) Text(question, def string, validate func(string) error) (string, error) {
	for {
		if def != "" {
			fmt.Fprintf(p.w, "%s [%s]: ", question, def)
		} else {
			fmt.Fprintf(p.w, "%s: ", question)
		}
		line, err := p.readLine()
		if err != nil {
			return "", err
		}
		answer := line
		if answer == "" {
			answer = def
		}
		if validate != nil {
			if err := validate(answer); err != nil {
				fmt.Fprintf(p.w, "  %v\n", err)
				continue
			}
		}
		return answer, nil
	}
}

// YesNo asks question with def as the default; a blank answer takes def.
// Anything other than a recognised yes or no re-asks.
func (p *Prompter) YesNo(question string, def bool) (bool, error) {
	brackets := "y/N"
	if def {
		brackets = "Y/n"
	}
	for {
		fmt.Fprintf(p.w, "%s [%s]: ", question, brackets)
		line, err := p.readLine()
		if err != nil {
			return false, err
		}
		switch strings.ToLower(line) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(p.w, "  please answer y or n")
		}
	}
}

// Choice asks the developer to pick one of options (typed as the option's
// text or its 1-based number), showing def as the default on a blank
// answer. def must be one of options.
func (p *Prompter) Choice(question string, options []string, def string) (string, error) {
	for {
		fmt.Fprintf(p.w, "%s:\n", question)
		for i, o := range options {
			fmt.Fprintf(p.w, "  %d) %s\n", i+1, o)
		}
		fmt.Fprintf(p.w, "Choice [%s]: ", def)
		line, err := p.readLine()
		if err != nil {
			return "", err
		}
		if line == "" {
			return def, nil
		}
		if n, err := strconv.Atoi(line); err == nil {
			if n >= 1 && n <= len(options) {
				return options[n-1], nil
			}
		} else {
			for _, o := range options {
				if strings.EqualFold(o, line) {
					return o, nil
				}
			}
		}
		fmt.Fprintf(p.w, "  please enter one of: %s\n", strings.Join(options, ", "))
	}
}
