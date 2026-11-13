package investigation

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"jumper/internal/errs"
	"jumper/internal/redact"
)

// AppendAudit appends one entry to root/audit/YYYY-MM-DD.md, creating the file
// with its heading on first use. Audit files use the same bullet format as an
// investigation timeline.
func AppendAudit(root string, e Entry, red *redact.Redactor, now time.Time) error {
	const op = "investigation.append_audit"
	dir := filepath.Join(root, dirAudit)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(errs.Internal, op, err)
	}
	date := now.Format("2006-01-02")
	path := filepath.Join(dir, date+".md")

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	switch {
	case err == nil:
		header := "# Audit " + date + "\n\n" + headingTimeline + "\n"
		_, werr := f.WriteString(header)
		cerr := f.Close()
		if werr == nil {
			werr = cerr
		}
		if werr != nil {
			return errs.Wrap(errs.Internal, op, werr)
		}
	case errors.Is(err, fs.ErrExist):
		// Already exists; append below.
	default:
		return errs.Wrap(errs.Internal, op, err)
	}
	return AppendEntry(path, e, red)
}
