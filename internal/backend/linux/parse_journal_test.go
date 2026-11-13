package linux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

const journalSample = `{"__REALTIME_TIMESTAMP":"1794512467000000","PRIORITY":"3","SYSLOG_IDENTIFIER":"nginx","_PID":"2290","_SYSTEMD_UNIT":"nginx.service","MESSAGE":"bind() to 0.0.0.0:443 failed (98: Address already in use)"}`

func TestParseJournalLineSample(t *testing.T) {
	e, err := ParseJournalLine([]byte(journalSample))
	if err != nil {
		t.Fatalf("ParseJournalLine: %v", err)
	}
	want := Entry{
		Time:     time.UnixMicro(1794512467000000).UTC(),
		Priority: 3,
		Source:   "nginx",
		PID:      "2290",
		Unit:     "nginx.service",
		Message:  "bind() to 0.0.0.0:443 failed (98: Address already in use)",
	}
	if e.Time != want.Time {
		t.Errorf("Time = %v, want %v", e.Time, want.Time)
	}
	if e.Priority != want.Priority || e.Source != want.Source || e.PID != want.PID {
		t.Errorf("entry = %+v, want %+v", e, want)
	}
	if e.Unit != want.Unit || e.Message != want.Message {
		t.Errorf("entry = %+v, want %+v", e, want)
	}
}

func TestParseJournalLineShapes(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		want  Entry
		isErr bool
	}{
		{
			name: "byte array message",
			line: `{"__REALTIME_TIMESTAMP":"1794512469000000","PRIORITY":"3","_COMM":"kernel","_TRANSPORT":"kernel","MESSAGE":[110,103,105,110,120,58,32,255,10]}`,
			want: Entry{
				Time:     time.UnixMicro(1794512469000000).UTC(),
				Priority: 3,
				Source:   "kernel",
				Message:  "nginx: \uFFFD",
			},
		},
		{
			name: "null message",
			line: `{"__REALTIME_TIMESTAMP":"1794512470000000","PRIORITY":"6","SYSLOG_IDENTIFIER":"sshd","MESSAGE":null}`,
			want: Entry{
				Time:     time.UnixMicro(1794512470000000).UTC(),
				Priority: 6,
				Source:   "sshd",
			},
		},
		{
			name: "missing priority defaults to info",
			line: `{"__REALTIME_TIMESTAMP":"1794512470000000","SYSLOG_IDENTIFIER":"sshd","MESSAGE":"x"}`,
			want: Entry{
				Time:     time.UnixMicro(1794512470000000).UTC(),
				Priority: 6,
				Source:   "sshd",
				Message:  "x",
			},
		},
		{
			name: "array of strings takes the first",
			line: `{"__REALTIME_TIMESTAMP":"1794512472000000","PRIORITY":"6","SYSLOG_IDENTIFIER":"backup","UNIT":"backup.service","_PID":"3311","MESSAGE":["first","second"]}`,
			want: Entry{
				Time:     time.UnixMicro(1794512472000000).UTC(),
				Priority: 6,
				Source:   "backup",
				PID:      "3311",
				Unit:     "backup.service",
				Message:  "first",
			},
		},
		{
			name: "source falls back to question mark",
			line: `{"__REALTIME_TIMESTAMP":"1794512473000000","PRIORITY":"7","PID":"4022","MESSAGE":"x"}`,
			want: Entry{
				Time:     time.UnixMicro(1794512473000000).UTC(),
				Priority: 7,
				Source:   "?",
				PID:      "4022",
				Message:  "x",
			},
		},
		{
			name: "kernel transport without identifiers",
			line: `{"__REALTIME_TIMESTAMP":"1794512473000000","_TRANSPORT":"kernel","MESSAGE":"x"}`,
			want: Entry{
				Time:     time.UnixMicro(1794512473000000).UTC(),
				Priority: 6,
				Source:   "kernel",
				Message:  "x",
			},
		},
		{
			name: "multi-line message keeps inner newlines and drops trailing ones",
			line: `{"__REALTIME_TIMESTAMP":"1794512471000000","PRIORITY":"2","SYSLOG_IDENTIFIER":"nginx","MESSAGE":"a\nb\n\n"}`,
			want: Entry{
				Time:     time.UnixMicro(1794512471000000).UTC(),
				Priority: 2,
				Source:   "nginx",
				Message:  "a\nb",
			},
		},
		{
			name:  "truncated json",
			line:  `{"__REALTIME_TIMESTAMP":"1794512474000000","MESS`,
			isErr: true,
		},
		{
			name:  "missing timestamp",
			line:  `{"PRIORITY":"3","MESSAGE":"x"}`,
			isErr: true,
		},
		{
			name:  "empty line",
			line:  "",
			isErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseJournalLine([]byte(tt.line))
			if tt.isErr {
				if err == nil {
					t.Fatalf("ParseJournalLine(%q) = %+v, want error", tt.line, got)
				}
				if !errs.Is(err, errs.Parse) {
					t.Errorf("kind = %s, want %s (%v)", errs.KindOf(err), errs.Parse, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseJournalLine: %v", err)
			}
			if got.Time != tt.want.Time {
				t.Errorf("Time = %v, want %v", got.Time, tt.want.Time)
			}
			got.Time = time.Time{}
			tt.want.Time = time.Time{}
			if got != tt.want {
				t.Errorf("entry = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseJournalCursor(t *testing.T) {
	e, err := ParseJournalLine([]byte(`{"__REALTIME_TIMESTAMP":"1794512467000000","__CURSOR":"s=1;i=1a2b","MESSAGE":"x"}`))
	if err != nil {
		t.Fatalf("ParseJournalLine: %v", err)
	}
	if e.Cursor != "s=1;i=1a2b" {
		t.Errorf("Cursor = %q, want %q", e.Cursor, "s=1;i=1a2b")
	}
}

func TestParseJournalFile(t *testing.T) {
	path := filepath.Join("..", "..", "..", "testdata", "linux", "journal", "rocky9", "nginx-fail.jsonl")
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	entries, skipped := ParseJournal(out)
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(entries) != 7 {
		t.Fatalf("entries = %d, want 7", len(entries))
	}
	if entries[0].Message != "bind() to 0.0.0.0:443 failed (98: Address already in use)" {
		t.Errorf("first message = %q", entries[0].Message)
	}
	if entries[2].Source != "kernel" || entries[2].Message != "nginx: failed to read: \uFFFD" {
		t.Errorf("byte-array entry = %+v", entries[2])
	}
	if entries[3].Message != "" || entries[3].Priority != 6 {
		t.Errorf("null-message entry = %+v", entries[3])
	}
	if entries[4].Message != "upstream timed out\nwhile reading response header from upstream" {
		t.Errorf("multi-line message = %q", entries[4].Message)
	}
	if entries[5].Message != "first line of a repeated field" || entries[5].Unit != "backup.service" {
		t.Errorf("repeated-field entry = %+v", entries[5])
	}
	if entries[6].Source != "?" || entries[6].PID != "4022" {
		t.Errorf("fallback entry = %+v", entries[6])
	}
	for i := 1; i < len(entries); i++ {
		if !entries[i].Time.After(entries[i-1].Time) {
			t.Errorf("entries out of order at %d", i)
		}
	}
}

func TestParseJournalSkipsBlankLinesSilently(t *testing.T) {
	out := []byte("\n" + journalSample + "\n\n   \n{\"bad\":\n")
	entries, skipped := ParseJournal(out)
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1", len(entries))
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
}

func TestPriorityName(t *testing.T) {
	want := []string{"emerg", "alert", "crit", "err", "warn", "notice", "info", "debug"}
	for p, name := range want {
		if got := PriorityName(p); got != name {
			t.Errorf("PriorityName(%d) = %q, want %q", p, got, name)
		}
	}
	if got := PriorityName(9); got != "info" {
		t.Errorf("PriorityName(9) = %q, want info", got)
	}
}

func TestPriorityStatus(t *testing.T) {
	tests := []struct {
		p    int
		want resource.Status
	}{
		{0, resource.StatusCrit},
		{1, resource.StatusCrit},
		{2, resource.StatusCrit},
		{3, resource.StatusCrit},
		{4, resource.StatusWarn},
		{5, resource.StatusOK},
		{6, resource.StatusOK},
		{7, resource.StatusOK},
	}
	for _, tt := range tests {
		if got := PriorityStatus(tt.p); got != tt.want {
			t.Errorf("PriorityStatus(%d) = %v, want %v", tt.p, got, tt.want)
		}
	}
}

func TestJournalCommand(t *testing.T) {
	now := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	window := resource.TimeWindow{From: now.Add(-time.Hour), To: now, Label: "-1h"}
	tests := []struct {
		name string
		opts JournalOptions
		want string
	}{
		{
			name: "unit and window",
			opts: JournalOptions{Unit: "nginx.service", MaxPriority: 4, Window: window, Now: now},
			want: "journalctl -o json --no-pager -u nginx.service -p 4 --since @1794484800 --until @1794488400 -n 500",
		},
		{
			name: "follow drops until and caps lines",
			opts: JournalOptions{Unit: "nginx.service", MaxPriority: 7, Window: resource.TimeWindow{From: now.Add(-time.Hour), ToNow: true}, Now: now, Follow: true},
			want: "journalctl -o json --no-pager -u nginx.service --since @1794484800 -n 50 -f",
		},
		{
			name: "kernel without window",
			opts: JournalOptions{Kernel: true, MaxPriority: 3, Now: now},
			want: "journalctl -o json --no-pager -p 3 -n 500 -k",
		},
		{
			name: "explicit limit",
			opts: JournalOptions{Limit: 1000, MaxPriority: 7, Window: resource.TimeWindow{From: now.Add(-time.Hour), ToNow: true}, Now: now},
			want: "journalctl -o json --no-pager --since @1794484800 --until @1794488400 -n 1000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JournalCommand(tt.opts); got != tt.want {
				t.Errorf("JournalCommand() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestJournalCommandQuotesUnit(t *testing.T) {
	got := JournalCommand(JournalOptions{Unit: "weird unit; rm -rf /", MaxPriority: 7})
	if !strings.Contains(got, `'weird unit; rm -rf /'`) {
		t.Errorf("unit not quoted: %s", got)
	}
}
