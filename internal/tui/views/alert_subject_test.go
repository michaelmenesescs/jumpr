package views

import (
	"testing"
	"time"

	"jumper/internal/backend/fake"
	"jumper/internal/resource"
)

// TestAlertTableSubjectCarriesAlertName: the knowledge panel asks for the alert
// name, not the row's fingerprint ref, so the subject must carry alertname.
func TestAlertTableSubjectCarriesAlertName(t *testing.T) {
	b := fake.New(fake.Options{Now: func() time.Time { return fixedNow }})
	var spec resource.KindSpec
	for _, s := range b.Kinds() {
		if s.Name == "alert" {
			spec = s
		}
	}
	if spec.Name == "" {
		t.Fatal("fake backend has no alert kind")
	}
	tbl := drain(t, NewTable(testEnv(), b, spec, resource.Query{Kind: "alert"}))
	if len(tbl.view) == 0 {
		t.Fatal("alert table has no rows")
	}
	subj := tbl.Subject()
	want := tbl.view[0].Labels["alertname"]
	if want == "" {
		t.Fatal("first alert row has no alertname label")
	}
	if subj.Alert != want {
		t.Fatalf("Subject().Alert = %q, want %q", subj.Alert, want)
	}
}
