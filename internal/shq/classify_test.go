package shq

import (
	"strings"
	"testing"

	"jumpr/internal/errs"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		rc     int
		stderr string
		sudo   bool
		kind   errs.Kind
	}{
		{"ok", 0, "", false, ""},
		{"plain failure", 1, "boom", true, ""},
		{"sudo password", 1, "sudo: a password is required", true, errs.Sudo},
		{"sudo terminal", 1, "sudo: a terminal is required to read the password", true, errs.Sudo},
		{"sudoers", 1, "bob is not in the sudoers file.", true, errs.Sudo},
		{"sudo upper", 1, "SUDO: A PASSWORD IS REQUIRED", true, errs.Sudo},
		{"sudo ignored when no sudo", 1, "sudo: a password is required", false, ""},
		{"rc127", 127, "", false, errs.NotAvailable},
		{"not found text", 1, "sh: 1: foo: command not found", false, errs.NotAvailable},
		{"rc126", 126, "sh: x: Permission denied", false, errs.NotAvailable},
	}
	for _, c := range cases {
		err := Classify(c.rc, c.stderr, c.sudo)
		if c.kind == "" {
			if err != nil {
				t.Errorf("%s: got %v want nil", c.name, err)
			}
			continue
		}
		if errs.KindOf(err) != c.kind {
			t.Errorf("%s: kind %v want %v", c.name, errs.KindOf(err), c.kind)
		}
	}
}

func TestClassifyMessage(t *testing.T) {
	err := Classify(1, "warning: x\n  sudo: a password is required  \nmore", true)
	var e *errs.Error
	if !asErr(err, &e) || e.Msg != "sudo: a password is required" {
		t.Fatalf("msg %+v", e)
	}
	err = Classify(127, strings.Repeat("é", 500), false)
	asErr(err, &e)
	if len([]rune(e.Msg)) != 200 {
		t.Fatalf("len %d", len([]rune(e.Msg)))
	}
}

func asErr(err error, target **errs.Error) bool {
	e, ok := err.(*errs.Error)
	*target = e
	return ok
}
