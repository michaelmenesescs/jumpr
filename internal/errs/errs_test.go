package errs

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestErrorFormat(t *testing.T) {
	cause := errors.New("connection refused")
	cases := []struct {
		name string
		err  *Error
		want string
	}{
		{
			name: "all parts",
			err:  &Error{Kind: Unreachable, Op: "ssh.dial", Host: "web-01", Msg: "cannot reach host", Err: cause},
			want: "ssh.dial: web-01: cannot reach host: connection refused",
		},
		{
			name: "no host",
			err:  &Error{Kind: Remote, Op: "ssh.exec", Msg: "exit status 1", Err: cause},
			want: "ssh.exec: exit status 1: connection refused",
		},
		{
			name: "no op",
			err:  &Error{Kind: Auth, Host: "web-01", Msg: "unable to authenticate"},
			want: "web-01: unable to authenticate",
		},
		{
			name: "only message",
			err:  New(Config, "", "bad selector"),
			want: "bad selector",
		},
		{
			name: "only kind",
			err:  &Error{Kind: Timeout},
			want: "timeout",
		},
		{
			name: "kind and cause, no message",
			err:  Wrap(Sudo, "ssh.exec", cause),
			want: "ssh.exec: connection refused",
		},
		{
			name: "host and cause, no message",
			err:  Wrap(Parse, "journal.parse", cause).WithHost("db-01"),
			want: "journal.parse: db-01: connection refused",
		},
		{
			name: "cause only",
			err:  Wrap(Remote, "", cause),
			want: "connection refused",
		},
		{
			name: "everything empty except kind",
			err:  &Error{Kind: Canceled},
			want: "canceled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWrapNilCause(t *testing.T) {
	e := Wrap(Internal, "test.op", nil)
	if e == nil {
		t.Fatal("Wrap returned nil")
	}
	if e.Err != nil {
		t.Fatalf("Wrap(kind, op, nil).Err = %v, want nil", e.Err)
	}
	// With no message and no cause the kind is the message.
	if got, want := e.Error(), "test.op: internal"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestNew(t *testing.T) {
	e := New(Policy, "bus.propose", "denied")
	if e.Kind != Policy || e.Op != "bus.propose" || e.Msg != "denied" || e.Err != nil {
		t.Fatalf("New() = %+v", e)
	}
}

func TestUnwrap(t *testing.T) {
	cause := errors.New("boom")
	e := Wrap(Remote, "op", cause)
	if !errors.Is(e, cause) {
		t.Fatal("errors.Is did not find the wrapped cause")
	}
	if got := errors.Unwrap(e); got != cause {
		t.Fatalf("Unwrap() = %v, want %v", got, cause)
	}
}

func TestWithHostIsCopy(t *testing.T) {
	orig := New(Auth, "ssh.dial", "no supported methods remain")
	with := orig.WithHost("web-01")
	if with == orig {
		t.Fatal("WithHost returned the receiver")
	}
	if got, want := with.Host, "web-01"; got != want {
		t.Fatalf("copy Host = %q, want %q", got, want)
	}
	if orig.Host != "" {
		t.Fatalf("receiver mutated: Host = %q", orig.Host)
	}
	// Further edits to the copy must not leak back.
	with.Msg = "changed"
	if orig.Msg != "no supported methods remain" {
		t.Fatalf("receiver mutated through copy: Msg = %q", orig.Msg)
	}
}

func TestKindOf(t *testing.T) {
	inner := New(Timeout, "ssh.exec", "deadline")
	cases := []struct {
		name string
		err  error
		want Kind
	}{
		{"nil", nil, ""},
		{"error", New(Sudo, "ssh.exec", "password required"), Sudo},
		{
			"wrapped error with fmt.Errorf",
			fmt.Errorf("load failed: %w", inner),
			Timeout,
		},
		{
			"double wrapped error",
			fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", New(Parse, "p", "bad"))),
			Parse,
		},
		{"deadline exceeded", context.DeadlineExceeded, Timeout},
		{"wrapped deadline exceeded", fmt.Errorf("dial: %w", context.DeadlineExceeded), Timeout},
		{"canceled", context.Canceled, Canceled},
		{"wrapped canceled", fmt.Errorf("run: %w", context.Canceled), Canceled},
		{"plain error", errors.New("boom"), Internal},
		{"wrapped plain error", fmt.Errorf("x: %w", errors.New("boom")), Internal},
		{"kind-less error struct", &Error{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindOf(tc.err); got != tc.want {
				t.Fatalf("KindOf() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIs(t *testing.T) {
	err := fmt.Errorf("op: %w", New(HostKey, "ssh.dial", "key mismatch"))
	if !Is(err, HostKey) {
		t.Fatal("Is(err, HostKey) = false")
	}
	if Is(err, Auth) {
		t.Fatal("Is(err, Auth) = true")
	}
	if Is(nil, HostKey) {
		t.Fatal("Is(nil, HostKey) = true")
	}
	// Is is exactly KindOf(err) == kind, and KindOf(nil) is "".
	if !Is(nil, "") {
		t.Fatal(`Is(nil, "") = false`)
	}
}
