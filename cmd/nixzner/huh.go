package main

import (
	"errors"
	"io"

	"github.com/charmbracelet/huh"
)

// huhPrompter asks on the terminal, drawing on out so stdout stays
// reserved for the result line.
type huhPrompter struct {
	out io.Writer
}

func (h huhPrompter) Input(title, initial string, validate func(string) error) (string, error) {
	v := initial
	err := h.run(huh.NewInput().Title(title).Value(&v).Validate(validate))
	return v, err
}

func (h huhPrompter) Select(title string, options []option) (string, error) {
	var v string
	opts := make([]huh.Option[string], len(options))
	for i, o := range options {
		opts[i] = huh.NewOption(o.Label, o.Value)
	}
	err := h.run(huh.NewSelect[string]().Title(title).Options(opts...).Height(12).Value(&v))
	return v, err
}

func (h huhPrompter) Confirm(title, summary string) (bool, error) {
	var ok bool
	err := h.run(huh.NewConfirm().Title(title).Description(summary).Affirmative("Create").Negative("Cancel").Value(&ok))
	return ok, err
}

func (h huhPrompter) run(field huh.Field) error {
	err := huh.NewForm(huh.NewGroup(field)).WithOutput(h.out).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return errAborted
	}
	return err
}
