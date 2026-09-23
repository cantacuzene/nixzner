package main

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"nixzner/internal/hetzner"
)

// fakePrompter answers from a script keyed by prompt title. A missing
// answer accepts the prompt's initial value.
type fakePrompter struct {
	script     map[string][]string
	confirm    bool
	asked      []string
	initial    map[string]string
	offered    map[string][]option
	validators map[string]func(string) error
	summary    string
}

func newFakePrompter(script map[string][]string) *fakePrompter {
	return &fakePrompter{
		script: script, confirm: true,
		initial: map[string]string{}, offered: map[string][]option{}, validators: map[string]func(string) error{},
	}
}

func (f *fakePrompter) next(title, fallback string) string {
	f.asked = append(f.asked, title)
	answers := f.script[title]
	if len(answers) == 0 {
		return fallback
	}
	f.script[title] = answers[1:]
	return answers[0]
}

func (f *fakePrompter) Input(title, initial string, validate func(string) error) (string, error) {
	f.initial[title] = initial
	f.validators[title] = validate
	v := f.next(title, initial)
	return v, validate(v)
}

func (f *fakePrompter) Select(title string, options []option) (string, error) {
	f.offered[title] = options
	return f.next(title, ""), nil
}

func (f *fakePrompter) Confirm(title, summary string) (bool, error) {
	f.asked = append(f.asked, title)
	f.summary = summary
	return f.confirm, nil
}

type fakeCatalog struct {
	locations []hetzner.Location
	offers    map[string][]hetzner.ServerTypeOffer
	sshKeys   []string
}

func (c fakeCatalog) Locations(context.Context) ([]hetzner.Location, error) { return c.locations, nil }
func (c fakeCatalog) ServerTypesInStock(_ context.Context, loc string) ([]hetzner.ServerTypeOffer, error) {
	return c.offers[loc], nil
}
func (c fakeCatalog) SSHKeyNames(context.Context) ([]string, error) { return c.sshKeys, nil }

func testCatalog() fakeCatalog {
	return fakeCatalog{
		locations: []hetzner.Location{{Name: "fsn1", City: "Falkenstein", Country: "DE"}, {Name: "nbg1", City: "Nuremberg", Country: "DE"}},
		offers: map[string][]hetzner.ServerTypeOffer{
			"nbg1": {{Name: "cax11", Architecture: "arm", Cores: 2, MemoryGB: 4, HourlyGross: 0.0096}},
		},
		sshKeys: []string{"admin", "laptop"},
	}
}

func TestPromptAsksForEverythingInOrder(t *testing.T) {
	p := newFakePrompter(map[string][]string{
		"Server name": {"web1"}, "Location": {"nbg1"}, "Server type": {"cax11"}, "SSH key": {"laptop"},
	})
	var a answers
	if err := promptMissing(context.Background(), p, testCatalog(), &a, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	wantAsked := []string{"Server name", "Location", "Server type", "SSH key", "Flake", "Create this server?"}
	if !slices.Equal(p.asked, wantAsked) {
		t.Errorf("asked %v, want %v", p.asked, wantAsked)
	}
	want := answers{Name: "web1", Location: "nbg1", ServerType: "cax11", SSHKey: "laptop", Flake: ".#web1"}
	if a != want {
		t.Errorf("answers = %+v, want %+v", a, want)
	}
	for _, s := range []string{"web1", "nbg1", "cax11", "laptop", ".#web1"} {
		if !strings.Contains(p.summary, s) {
			t.Errorf("summary %q does not mention %q", p.summary, s)
		}
	}
}

func TestPromptAsksOnlyForMissing(t *testing.T) {
	p := newFakePrompter(map[string][]string{"Location": {"nbg1"}, "Server type": {"cax11"}})
	a := answers{Name: "web1", SSHKey: "admin", Flake: ".#web1"}
	if err := promptMissing(context.Background(), p, testCatalog(), &a, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"Location", "Server type", "Create this server?"}; !slices.Equal(p.asked, want) {
		t.Errorf("asked %v, want %v", p.asked, want)
	}
}

func TestPromptAsksNothingWhenComplete(t *testing.T) {
	p := newFakePrompter(nil)
	a := answers{Name: "web1", Location: "nbg1", ServerType: "cax11", SSHKey: "admin", Flake: ".#web1"}
	if err := promptMissing(context.Background(), p, testCatalog(), &a, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 0 {
		t.Errorf("asked %v, want nothing", p.asked)
	}
}

func TestPromptOptionLabels(t *testing.T) {
	p := newFakePrompter(map[string][]string{"Location": {"nbg1"}, "Server type": {"cax11"}})
	a := answers{Name: "web1", SSHKey: "admin", Flake: ".#web1"}
	if err := promptMissing(context.Background(), p, testCatalog(), &a, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := p.offered["Location"][1]; got.Value != "nbg1" || got.Label != "nbg1   Nuremberg, DE" {
		t.Errorf("location option = %+v", got)
	}
	if got := p.offered["Server type"][0]; got.Value != "cax11" || got.Label != "cax11    arm   2 vCPU    4 GB  €0.0096/h" {
		t.Errorf("server type option = %+v", got)
	}
}

func TestPromptReasksLocationWhenNothingInStock(t *testing.T) {
	p := newFakePrompter(map[string][]string{"Location": {"fsn1", "nbg1"}, "Server type": {"cax11"}})
	a := answers{Name: "web1", SSHKey: "admin", Flake: ".#web1"}
	var stderr bytes.Buffer
	if err := promptMissing(context.Background(), p, testCatalog(), &a, &stderr); err != nil {
		t.Fatal(err)
	}
	if a.Location != "nbg1" {
		t.Errorf("location = %q, want nbg1", a.Location)
	}
	if !strings.Contains(stderr.String(), "no server type is in stock in fsn1") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestPromptErrorsWhenGivenLocationHasNoStock(t *testing.T) {
	p := newFakePrompter(nil)
	a := answers{Name: "web1", Location: "fsn1", SSHKey: "admin", Flake: ".#web1"}
	err := promptMissing(context.Background(), p, testCatalog(), &a, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no server type is in stock in fsn1") {
		t.Fatalf("err = %v", err)
	}
}

func TestPromptErrorsWithoutSSHKeys(t *testing.T) {
	cat := testCatalog()
	cat.sshKeys = nil
	p := newFakePrompter(nil)
	a := answers{Name: "web1", Location: "nbg1", ServerType: "cax11", Flake: ".#web1"}
	err := promptMissing(context.Background(), p, cat, &a, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no SSH keys") {
		t.Fatalf("err = %v", err)
	}
}

func TestPromptDeclinedConfirmationAborts(t *testing.T) {
	p := newFakePrompter(map[string][]string{"Server name": {"web1"}})
	p.confirm = false
	a := answers{Location: "nbg1", ServerType: "cax11", SSHKey: "admin", Flake: ".#web1"}
	err := promptMissing(context.Background(), p, testCatalog(), &a, &bytes.Buffer{})
	if !errors.Is(err, errAborted) {
		t.Fatalf("err = %v, want errAborted", err)
	}
}

func TestPromptValidatesNameAndFlake(t *testing.T) {
	p := newFakePrompter(map[string][]string{"Server name": {"web1"}})
	a := answers{Location: "nbg1", ServerType: "cax11", SSHKey: "admin"}
	if err := promptMissing(context.Background(), p, testCatalog(), &a, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if p.validators["Server name"]("Web_1") == nil {
		t.Error("server name validator accepted Web_1")
	}
	if p.validators["Flake"]("web1") == nil {
		t.Error("flake validator accepted web1 (no #host)")
	}
}
